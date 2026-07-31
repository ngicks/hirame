#!/bin/sh
# The whole end-to-end run, inside one user namespace.
#
# Not meant to be invoked directly: run-e2e.sh is the entrypoint and hands this
# to pnp-enter. It is one script rather than seven because namespaces are not
# re-enterable here -- there is no nsenter, and a second pnp-enter is a
# different user namespace, so a stack started in one invocation does not exist
# in the next.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
. "$HERE/lib/pnp-lib.sh"

RUN=/run/pnp/e2e
UNITS=$RUN/units
UNITS_FAIL=$RUN/units-migration-failure
UNITS_QUOTA=$RUN/units-eviction-quota
LOGS=$RUN/logs
RESULTS=$RUN/results.tsv
# The deployment's two configuration directories (D-009), and the harness has
# to reproduce the split rather than pick one: the Quadlet units are user units
# naming %h/.config/hirame/..., which the supervisor expands against this
# namespace's HOME, while overwatch.json belongs to the *system* daemon and
# lives in /etc/hirame exactly as deploy/systemd/overwatch.service names it.
CONFIG=$HOME/.config/hirame
SYSCONFIG=/etc/hirame
DOCS=/srv/documents
# The host runtime directory deploy/systemd/overwatch.service declares as
# RuntimeDirectory=, and the bind source indexer.container mounts. An absolute
# /run path in both scopes: the daemon is a system service either way.
OVERWATCH_RUN=/run/overwatch/hirame
SUP=$HERE/lib/quadlet-supervisor.py

# Host networking makes ports a global resource. These are the deployed values
# and not an offset block on purpose: apps/web-gui/Caddyfile hard-codes :8080
# and reverse_proxy 127.0.0.1:8081, so moving them would take the deployed
# Caddyfile out of the test. Every one is required free before anything starts.
PORT_PG=5432
PORT_TIKA=9998
PORT_GAHAKU=9000
PORT_S3=7070
PORT_API=8081
PORT_WEB=8080

API=http://127.0.0.1:$PORT_API
WEB=http://127.0.0.1:$PORT_WEB

VOLUME_UNITS='hirame-db-volume.service hirame-objectstore-volume.service'
# overwatch.service is absent on purpose: it is not a Quadlet unit any more, so
# the supervisor has nothing to generate for it. start_fake_overwatch below is
# what stands in for it, as a host process — the shape the deployment now has.
INFRA_UNITS='postgres.service tika.service gahaku.service versitygw.service versitygw-bootstrap.service'
APP_UNITS='hirame-migrate.service search-api.service indexer.service web-gui.service'

# --------------------------------------------------------------------------
# assertions and the summary table
# --------------------------------------------------------------------------

SCENARIO_FAILED=0

ok() { printf '  PASS   %s\n' "$*"; }
bad() {
	printf '  FAIL   %s\n' "$*"
	SCENARIO_FAILED=1
}

check_eq() { # check_eq <what> <expected> <actual>
	if [ "$2" = "$3" ]; then ok "$1 = $3"; else bad "$1: expected [$2], got [$3]"; fi
}

check_contains() { # check_contains <what> <needle> <haystack>
	case "$3" in
	*"$2"*) ok "$1 contains '$2'" ;;
	*) bad "$1: '$2' not found in [$(printf '%.200s' "$3")]" ;;
	esac
}

check_true() { # check_true <what> <cmd...>
	_what=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$_what"; else bad "$_what"; fi
}

run_scenario() { # run_scenario <id> <title> <fn>
	printf '\n################ %s  %s ################\n' "$1" "$2"
	SCENARIO_FAILED=0
	set +e
	"$3"
	[ $? -eq 0 ] || SCENARIO_FAILED=1
	set -e
	if [ "$SCENARIO_FAILED" -eq 0 ]; then _st=PASS; else _st=FAIL; fi
	printf '%s\t%s\t%s\n' "$1" "$_st" "$2" >>"$RESULTS"
	printf '################ %s: %s\n' "$1" "$_st"
}

# --------------------------------------------------------------------------
# service access
# --------------------------------------------------------------------------

sup() { "$SUP" "$@"; }

# sup_log runs the supervisor with its output captured *and* shown, so a
# scenario can assert on what a oneshot unit printed. The only place that
# matters is migrate, whose stdout is its report.
sup_log() { # sup_log <logfile> <args...>
	_log=$1
	shift
	# `if` rather than set +e/set -e: this is called from inside a scenario,
	# where errexit is already off, and restoring it there would abort the
	# whole run on the first failed assertion.
	if "$SUP" "$@" >"$_log" 2>&1; then _rc=0; else _rc=$?; fi
	cat "$_log"
	return $_rc
}

unit_state() { # unit_state <unit>
	python3 -c 'import json,sys
try:
    print(json.load(open(sys.argv[1])).get(sys.argv[2], {}).get("state", "absent"))
except OSError:
    print("absent")' /run/pnp/quadlet-supervisor.json "$1"
}

psql_q() { # psql_q <sql>  -> tab-separated rows, no header
	$PODMAN exec -e PGPASSWORD=hirame-e2e-not-a-secret postgres \
		psql -h 127.0.0.1 -U hirame -d hirame -tAqc "$1"
}

rpc() { # rpc <base-url> <fully.qualified.Service/Method> <json>  -> body
	curl -sS -X POST -H 'Content-Type: application/json' --data "$3" "$1/api/$2"
}

rpc_status() { # rpc_status <base-url> <procedure> <json>  -> "<http-code> <body>"
	curl -sS -o "$RUN/rpc.out" -w '%{http_code}' \
		-X POST -H 'Content-Type: application/json' --data "$3" "$1/api/$2"
}

# object_count counts what is actually in the thumbnail bucket. The
# objectstore volume is backed by a tmpfs this namespace mounted, so its
# contents are readable here directly -- a truer answer than asking the
# gateway that wrote them.
object_count() {
	_mp=$($PODMAN volume inspect hirame-objectstore --format '{{.Mountpoint}}')
	# .sgwtmp is versitygw's own scratch area, not an object.
	find "$_mp/hirame-thumbnails" -type f -not -path '*/.sgwtmp/*' 2>/dev/null |
		wc -l | tr -d ' '
}

# --------------------------------------------------------------------------
# fixtures
# --------------------------------------------------------------------------

JP_DOC=$DOCS/japanese-report.txt
PDF_DOC=$DOCS/render-probe.pdf

# The Japanese fixture exercises what D-012 was chosen for: a compound noun
# ("四半期経営会議"), a kana word, and a half-width sequence that only matches
# after NFKC normalization.
write_japanese_doc() { # write_japanese_doc <marker>
	cat >"$JP_DOC" <<EOF
四半期経営会議の議事録
本日の四半期経営会議では、売上高および営業利益について報告が行われた。
アパートの改修工事は来月着工の予定である。
ﾊﾝｶｸ ｶﾀｶﾅ の表記ゆれも索引対象とする。
改訂マーカー: $1
EOF
}

# A PDF is needed because Gahaku renders PDF and office formats only, and this
# has to be a document the renderer accepts. Hand-built rather than converted
# with soffice: the byte offsets in the xref table are the only fiddly part and
# python computes them exactly, whereas a LibreOffice round trip would add a
# multi-second dependency to a fixture.
write_pdf_doc() { # write_pdf_doc <text>
	python3 - "$PDF_DOC" "$1" <<'PY'
import sys

path, text = sys.argv[1], sys.argv[2]
stream = f"BT /F1 24 Tf 72 700 Td ({text}) Tj ET".encode()
objects = [
    b"<< /Type /Catalog /Pages 2 0 R >>",
    b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
    b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
    b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
    b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    b"<< /Length %d >>\nstream\n%s\nendstream" % (len(stream), stream),
]

out = bytearray(b"%PDF-1.4\n")
offsets = []
for i, body in enumerate(objects, start=1):
    offsets.append(len(out))
    out += b"%d 0 obj\n" % i + body + b"\nendobj\n"

xref = len(out)
out += b"xref\n0 %d\n" % (len(objects) + 1)
out += b"0000000000 65535 f \n"
for off in offsets:
    out += b"%010d 00000 n \n" % off
out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (
    len(objects) + 1,
    xref,
)
with open(path, "wb") as f:
    f.write(bytes(out))
PY
}

# search_hit returns the first hit for a query as compact JSON, or empty.
search_hit() { # search_hit <query> [base]
	rpc "${2:-$API}" hirame.v1.SearchService/Search \
		"$(printf '{"query":%s,"pageSize":5,"maxSnippets":3}' "$(json_string "$1")")" |
		jq -c '.hits[0] // empty'
}

json_string() { printf '%s' "$1" | jq -Rs .; }

wait_for_hit() { # wait_for_hit <seconds> <query>
	_n=$1
	while [ "$_n" -gt 0 ]; do
		if [ -n "$(search_hit "$2" 2>/dev/null)" ]; then return 0; fi
		_n=$((_n - 5))
		sleep 5
	done
	return 1
}

# --------------------------------------------------------------------------
# setup
# --------------------------------------------------------------------------

setup() {
	log "scratch tmpfs, unit directory, and configuration"
	pnp_scratch 6g
	rm -rf "$RUN"
	mkdir -p "$UNITS" "$UNITS_FAIL" "$UNITS_QUOTA" "$LOGS"
	: >"$RESULTS"

	for p in "$PORT_PG:postgres" "$PORT_TIKA:tika" "$PORT_GAHAKU:gahaku" \
		"$PORT_S3:versitygw" "$PORT_API:search-api" "$PORT_WEB:web-gui"; do
		pnp_require_port "${p%%:*}" "${p#*:}"
	done

	# The images are checked by the units that name them; these two are not
	# named by anything, so --no-build after a clean checkout would otherwise
	# fail inside `timeout` with a bare "No such file or directory".
	for b in overwatch fakeoverwatch; do
		[ -x "$HERE/bin/$b" ] ||
			die "$HERE/bin/$b is missing; run without --no-build"
	done

	# The units under test are the deployed ones, copied byte for byte; only
	# the drop-in directories beside them are the harness's. The checksum
	# below is the proof, and it runs on every start rather than on request.
	cp -a "$REPO/deploy/quadlet/." "$UNITS/"
	cp -a "$HERE/dropins/." "$UNITS/"
	cp -a "$UNITS/." "$UNITS_FAIL/"
	cp -a "$HERE/dropins-migration-failure/." "$UNITS_FAIL/"
	cp -a "$UNITS/." "$UNITS_QUOTA/"
	cp -a "$HERE/dropins-eviction-quota/." "$UNITS_QUOTA/"
	verify_units_unmodified

	install -d -m 0700 "$CONFIG"
	install -m 0644 "$HERE/env/hirame.env" "$CONFIG/hirame.env"
	install -m 0600 "$HERE/env/secrets.env" "$CONFIG/secrets.env"
	install -m 0600 "$HERE/env/broken-migration.env" "$CONFIG/broken-migration.env"
	install -m 0644 "$HERE/env/eviction-quota.env" "$CONFIG/eviction-quota.env"
	# 0644 because this one is bind-mounted and read by the container's own
	# uid, unlike the env files, which podman itself reads as --env-file.
	install -m 0644 "$REPO/deploy/config/postgresql.conf" "$CONFIG/postgresql.conf"
	install -d -m 0755 "$SYSCONFIG"
	install -m 0644 "$REPO/deploy/config/overwatch.json" "$SYSCONFIG/overwatch.json"
	# podman silently creates a *directory* for a bind source that does not
	# exist, which turns a missing config file into a service that started
	# with defaults rather than into an error.
	for f in hirame.env secrets.env broken-migration.env eviction-quota.env \
		postgresql.conf; do
		[ -f "$CONFIG/$f" ] || die "$CONFIG/$f is not a regular file"
	done
	[ -f "$SYSCONFIG/overwatch.json" ] ||
		die "$SYSCONFIG/overwatch.json is not a regular file"

	# The watched tree is a tmpfs of its own so each run starts from nothing
	# and no fixture is left in the repository checkout.
	mkdir -p "$DOCS"
	grep -q " $DOCS " /proc/self/mountinfo || pnp-mount tmpfs hirame-docs "$DOCS" size=256m
	rm -rf "${DOCS:?}/"* 2>/dev/null || true
	chmod 0755 "$DOCS"
}

verify_units_unmodified() {
	log "unit files under test are deploy/quadlet/, unmodified"
	_diff=$(cd "$REPO/deploy/quadlet" && find . -type f | sort | while read -r f; do
		cmp -s "$REPO/deploy/quadlet/$f" "$UNITS/$f" || echo "MODIFIED: $f"
	done)
	[ -z "$_diff" ] || die "$_diff"
	printf 'all %d unit files match deploy/quadlet/ byte for byte\n' \
		"$(find "$REPO/deploy/quadlet" -type f | wc -l)"
	printf 'test-profile drop-ins applied on top:\n'
	(cd "$UNITS" && find . -name '*.conf' | sort | sed 's/^/  /')
}

# probe_real_overwatch records, rather than assumes, why the stand-in daemon is
# used. It runs the deployed binary the way deploy/systemd/overwatch.service
# does -- as a host process, not a container -- so the refusal it records is the
# one a real install would get. It must run *before* start_fake_overwatch:
# both bind the same socket, and an "address already in use" would hide the
# fanotify error this exists to capture.
probe_real_overwatch() {
	log "probe: can the real overwatch daemon start here?"
	install -d -m 0750 "$OVERWATCH_RUN"
	set +e
	# Bounded, because a daemon that *did* start would serve forever: 124 is
	# timeout's own exit status and therefore the "it worked" outcome.
	timeout 30 "$HERE/bin/overwatch" server serve --config "$SYSCONFIG/overwatch.json" \
		>"$LOGS/overwatch-probe.log" 2>&1
	_rc=$?
	set -e
	printf 'exit=%d\n' "$_rc"
	sed 's/^/  | /' "$LOGS/overwatch-probe.log"
	if [ "$_rc" -eq 0 ] || [ "$_rc" -eq 124 ]; then
		warn "the real overwatch daemon started here; the stand-in in start_fake_overwatch is no longer needed"
	fi
}

# start_fake_overwatch stands in for deploy/systemd/overwatch.service. A plain
# background process, because that is what the unit runs: there is no image and
# no container to override any more, only a binary, a config file and a runtime
# directory.
start_fake_overwatch() {
	log "starting the stand-in overwatch daemon"
	# systemd creates this before ExecStart (RuntimeDirectory=), and the
	# ordering matters for the same reason there: podman silently creates a
	# missing bind source as an empty directory, which would leave the indexer
	# watching a path no socket ever appears in.
	install -d -m 0750 "$OVERWATCH_RUN"
	"$HERE/bin/fakeoverwatch" server serve --config "$SYSCONFIG/overwatch.json" \
		>"$LOGS/overwatch.log" 2>&1 &
	FAKE_OVERWATCH_PID=$!
	wait_for 30 "$HERE/bin/fakeoverwatch" client \
		--socket "$OVERWATCH_RUN/hirame.sock" status ||
		die "the stand-in overwatch daemon never answered on $OVERWATCH_RUN/hirame.sock"
	printf 'stand-in daemon pid=%s socket=%s\n' \
		"$FAKE_OVERWATCH_PID" "$OVERWATCH_RUN/hirame.sock"
}

teardown() {
	log "stopping everything"
	sup stop "$UNITS" $APP_UNITS $INFRA_UNITS >/dev/null 2>&1 || true
	[ -n "${FAKE_OVERWATCH_PID:-}" ] && kill "$FAKE_OVERWATCH_PID" 2>/dev/null
	return 0
}

# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------

setup
probe_real_overwatch
trap teardown EXIT INT TERM
start_fake_overwatch

for s in "$HERE"/scenarios/s*.sh; do . "$s"; done

run_scenario S1 'clean install: migration gate opens' scenario_s1
run_scenario S2 'migration failure blocks dependents' scenario_s2
run_scenario S3 'ingest to Japanese search' scenario_s3
run_scenario S4 'render and thumbnail cache' scenario_s4
run_scenario S5 'invalidation after modification' scenario_s5
run_scenario S6 'restart with an up-to-date schema' scenario_s6
run_scenario S7 'web entry point' scenario_s7
run_scenario S8 'thumbnail cache eviction' scenario_s8

log "unit states"
sup status

log "summary"
printf '%-4s  %-4s  %s\n' ID RESULT SCENARIO
sed 's/\t/|/g' "$RESULTS" | while IFS='|' read -r id st title; do
	printf '%-4s  %-4s  %s\n' "$id" "$st" "$title"
done
if grep -q '	FAIL	' "$RESULTS"; then
	printf '\nRESULT: FAIL\n'
	exit 1
fi
printf '\nRESULT: PASS\n'
