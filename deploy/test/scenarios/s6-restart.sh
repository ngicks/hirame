# S6 -- restart with an up-to-date schema.
#
# AGENTS.md: "restart with an up-to-date schema". A second boot over the same
# data must apply nothing, and the documents ingested before the restart must
# still be searchable without being extracted again.

scenario_s6() {
	log "S6: recording the pre-restart state"
	_migrations=$(psql_q "select count(*) || '/' || max(version) from schema_migrations")
	_extractions=$(psql_q "select count(*) || '/' || coalesce(max(extract(epoch from updated_at))::bigint, 0)
	                       from extracted_contents")
	_versions=$(psql_q 'select count(*) from content_versions')
	printf '  schema_migrations=%s extracted_contents=%s content_versions=%s\n' \
		"$_migrations" "$_extractions" "$_versions"

	log "S6: stopping everything"
	sup stop "$UNITS" $APP_UNITS $INFRA_UNITS
	check_true 'postgres is stopped' sh -c "! $PODMAN container exists postgres"
	check_true 'the API port is closed' sh -c "! curl -sf $API/healthz"

	log "S6: restarting against the same data directories"
	# The volumes and their tmpfs backing are untouched: only the containers
	# went away, which is what a `systemctl restart` of the whole stack does.
	sup start "$UNITS" $INFRA_UNITS
	sup_log "$LOGS/s6-migrate.log" start "$UNITS" $APP_UNITS

	check_eq 'hirame-migrate.service' started "$(unit_state hirame-migrate.service)"
	check_eq 'search-api.service' started "$(unit_state search-api.service)"
	check_eq 'indexer.service' started "$(unit_state indexer.service)"

	log "S6: what migrate reported this time"
	grep -E 'schema up to date|applied [0-9]+' "$LOGS/s6-migrate.log" | sed 's/^/  | /'
	check_true 'migrate reported the schema up to date' \
		grep -q 'schema up to date' "$LOGS/s6-migrate.log"
	check_true 'migrate applied nothing' \
		sh -c "! grep -qE '^applied [0-9]+' '$LOGS/s6-migrate.log'"

	check_eq 'schema_migrations unchanged' "$_migrations" \
		"$(psql_q "select count(*) || '/' || max(version) from schema_migrations")"

	log "S6: previously ingested documents are still searchable"
	check_true 'the Japanese document is still searchable' wait_for_hit 90 '四半期経営会議'
	check_true 'the PDF is still searchable' wait_for_hit 90 'kaitei'

	# Nothing was re-extracted: the rescan the restart triggers observes the
	# same size and mtime, so no ingest job is queued and no extraction row is
	# rewritten. A changed updated_at would mean the pipeline redid the work.
	sleep 20
	check_eq 'extracted_contents untouched' "$_extractions" \
		"$(psql_q "select count(*) || '/' || coalesce(max(extract(epoch from updated_at))::bigint, 0)
		           from extracted_contents")"
	check_eq 'no new content versions' "$_versions" \
		"$(psql_q 'select count(*) from content_versions')"
	return $SCENARIO_FAILED
}
