# S8 -- the thumbnail cache's two limits both fire (D-008).
#
# The maintenance job is a River periodic job with RunOnStart, so restarting
# the indexer is what triggers a pass -- no clock to wait out and no job row
# hand-written into River's schema. Each half is set up so the *deployed* limit
# is the one that acts:
#
#   age    the row is back-dated past S3_MAX_OBJECT_AGE, which stays 720h;
#   quota  the indexer is restarted with S3_MAX_CACHE_BYTES=1 through one extra
#          EnvironmentFile, the same mechanism S2 uses.

restart_indexer() { # restart_indexer <unit-dir>
	sup stop "$1" indexer.service >/dev/null 2>&1
	sup start "$1" indexer.service >/dev/null 2>&1
}

wait_for_empty_cache() { # wait_for_empty_cache <seconds>
	_n=$1
	while [ "$_n" -gt 0 ]; do
		[ "$(psql_q 'select count(*) from thumbnail_cache')" = 0 ] && return 0
		_n=$((_n - 5))
		sleep 5
	done
	return 1
}

# ensure_one_thumbnail renders a thumbnail for whatever the PDF's current
# version is, so each half starts from exactly one cached object.
ensure_one_thumbnail() {
	_ref=$(search_hit 'render probe' | jq -c '.ref')
	rpc_status "$API" hirame.v1.ThumbnailService/GetThumbnail \
		"$(printf '{"ref":%s,"pageNumber":1,"spec":{"format":"IMAGE_FORMAT_PNG"}}' "$_ref")" \
		>/dev/null
}

scenario_s8() {
	log "S8: age eviction"
	ensure_one_thumbnail
	check_eq 'one cached thumbnail to age out' 1 \
		"$(psql_q 'select count(*) from thumbnail_cache')"
	check_eq 'one object to age out' 1 "$(object_count)"

	# 800h is past the deployed S3_MAX_OBJECT_AGE of 720h. last_access_at moves
	# with it so the row cannot be mistaken for freshly used.
	psql_q "update thumbnail_cache
	        set created_at = now() - interval '800 hours',
	            last_access_at = now() - interval '800 hours'" | sed 's/^/  /'
	psql_q "select object_key, created_at from thumbnail_cache" | sed 's/^/  /'

	restart_indexer "$UNITS"
	check_true 'the aged row is evicted' wait_for_empty_cache 90
	check_eq 'the aged object is deleted' 0 "$(object_count)"

	log "S8: quota eviction"
	ensure_one_thumbnail
	check_eq 'one cached thumbnail to exceed the quota' 1 \
		"$(psql_q 'select count(*) from thumbnail_cache')"
	check_eq 'one object to exceed the quota' 1 "$(object_count)"
	printf '  cache holds %s bytes against a 1-byte quota\n' \
		"$(psql_q 'select coalesce(sum(size_bytes), 0) from thumbnail_cache')"

	restart_indexer "$UNITS_QUOTA"
	check_true 'the over-quota row is evicted' wait_for_empty_cache 90
	check_eq 'the over-quota object is deleted' 0 "$(object_count)"

	log "S8: restoring the deployed quota"
	restart_indexer "$UNITS"
	check_eq 'indexer.service' started "$(unit_state indexer.service)"
	return $SCENARIO_FAILED
}
