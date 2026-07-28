# S2 -- a failed migration must stop its dependents.
#
# AGENTS.md: "failure behavior for an unsuccessful migration", and the one
# assertion in the whole suite that distinguishes real enforcement from
# ordering that merely looks right (D-005).
#
# The dependents are stopped first: "did not start" proves nothing about a
# container that was already running.

scenario_s2() {
	log "S2: stopping the application units"
	sup stop "$UNITS" $APP_UNITS
	for c in systemd-search-api systemd-indexer systemd-web-gui; do
		check_true "$c is gone" sh -c "! $PODMAN container exists $c"
	done

	log "S2: starting them again with an unreachable DATABASE_URL for migrate only"
	if sup_log "$LOGS/s2-apps.log" start "$UNITS_FAIL" \
		hirame-migrate.service search-api.service indexer.service; then
		_rc=0
	else
		_rc=$?
	fi
	check_true 'supervisor reports failure' test "$_rc" -ne 0

	log "S2: unit states"
	sup status

	check_eq 'hirame-migrate.service' failed "$(unit_state hirame-migrate.service)"
	check_eq 'search-api.service' skipped "$(unit_state search-api.service)"
	check_eq 'indexer.service' skipped "$(unit_state indexer.service)"

	# The state table is the supervisor's own bookkeeping, so the containers
	# are checked independently: nothing may have been started.
	for c in systemd-search-api systemd-indexer; do
		check_true "$c was not started" sh -c "! $PODMAN container exists $c"
	done
	check_true 'the API port is not answering' sh -c "! curl -sf $API/healthz"

	log "S2: what migrate reported"
	grep -iE 'connect|refus|dial|error' "$LOGS/s2-apps.log" | head -5 | sed 's/^/  | /'

	log "S2: restoring the stack"
	sup stop "$UNITS_FAIL" hirame-migrate.service search-api.service indexer.service
	sup start "$UNITS" $APP_UNITS
	check_eq 'hirame-migrate.service after restore' started \
		"$(unit_state hirame-migrate.service)"
	check_eq 'search-api.service after restore' started "$(unit_state search-api.service)"
	return $SCENARIO_FAILED
}
