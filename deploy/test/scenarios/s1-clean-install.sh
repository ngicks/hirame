# S1 -- clean install.
#
# AGENTS.md: "first-time migration". From an empty database volume, the
# migration unit must run to completion and the applications must start after
# it, not beside it.

scenario_s1() {
	log "S1: discarding any previous volumes"
	sup stop "$UNITS" $APP_UNITS $INFRA_UNITS >/dev/null 2>&1 || true
	$PODMAN volume rm -f hirame-db hirame-objectstore >/dev/null 2>&1 || true
	rm -f /run/pnp/quadlet-supervisor.json

	log "S1: creating volumes and backing the chown-needing ones with tmpfs"
	sup start "$UNITS" $VOLUME_UNITS
	# The ParadeDB image su-execs to uid 999 and versitygw to its own user,
	# and both chown their data directory first. podman's volume directory is
	# on a filesystem an ancestor user namespace owns, which rejects that;
	# a tmpfs mounted here does not. The overwatch socket needs none of this:
	# it lives on /run, outside podman's volumes entirely, as it does in the
	# deployment (D-009).
	pnp_volume_tmpfs hirame-db 3g
	pnp_volume_tmpfs hirame-objectstore 1g

	log "S1: starting infrastructure"
	sup start "$UNITS" $INFRA_UNITS

	log "S1: starting the application units (migration gate first)"
	sup_log "$LOGS/s1-apps.log" start "$UNITS" $APP_UNITS

	log "S1: unit states"
	sup status

	check_eq 'hirame-migrate.service' started "$(unit_state hirame-migrate.service)"
	check_eq 'search-api.service' started "$(unit_state search-api.service)"
	check_eq 'indexer.service' started "$(unit_state indexer.service)"
	check_eq 'web-gui.service' started "$(unit_state web-gui.service)"

	# Ordering, from the supervisor's own trace rather than from wall time:
	# migrate has to be started before search-api is even attempted.
	_order=$(grep -E '^== starting (hirame-migrate|search-api)\.service' "$LOGS/s1-apps.log" |
		sed 's/^== starting //' | tr '\n' ' ')
	check_eq 'start order' 'hirame-migrate.service search-api.service ' "$_order"

	log "S1: schema present"
	_tables=$(psql_q "select string_agg(tablename, ',' order by tablename)
	                  from pg_tables where schemaname = 'public'")
	printf '  tables: %s\n' "$_tables"
	for t in mountpoints fs_observations reconcile_state content_versions \
		documents extracted_contents thumbnail_cache schema_migrations; do
		check_contains "public schema" "$t" "$_tables"
	done

	# The BM25 index is what D-012 is about; a schema that migrated but left
	# it out would still answer every table check above.
	check_eq 'extracted_contents_bm25 index' 1 \
		"$(psql_q "select count(*) from pg_indexes
		           where indexname = 'extracted_contents_bm25'")"
	# deploy/config/postgresql.conf overrides ParadeDB's generated file with
	# include_if_exists rather than replacing it, precisely so this survives.
	# Replacing it would leave a server that starts and a migration that fails
	# on CREATE INDEX ... USING bm25.
	check_contains 'shared_preload_libraries' pg_search \
		"$(psql_q 'show shared_preload_libraries')"
	# River's own schema is applied by the same migrate run.
	check_eq 'river_job table' 1 \
		"$(psql_q "select count(*) from pg_tables
		           where schemaname='public' and tablename='river_job'")"

	log "S1: applied migrations"
	psql_q "select version from schema_migrations order by version" | sed 's/^/  /'

	check_true 'search-api /healthz answers' \
		curl -sf "$API/healthz"
	return $SCENARIO_FAILED
}
