# S5 -- a changed document invalidates every thumbnail derived from the old
# bytes before anything new is served.
#
# The lifecycle requirement AGENTS.md states in full, and the reason
# DocumentRef carries a content version at all: the old ref must stop resolving
# rather than quietly returning an image rendered from bytes that no longer
# exist.

scenario_s5() {
	_old_ref=$S4_REF
	_old_version=$(printf '%s' "$_old_ref" | jq -r '.contentVersionId')
	_doc_id=$(printf '%s' "$_old_ref" | jq -r '.documentId')
	printf '  document %s, old content version %s\n' "$_doc_id" "$_old_version"
	check_true 'S4 left a ref to invalidate' test -n "$_old_version"
	check_eq 'a cached thumbnail exists to invalidate' 1 \
		"$(psql_q "select count(*) from thumbnail_cache tc
		           join content_versions cv on cv.id = tc.content_version_id
		           where cv.content_hash = '$_old_version'")"

	log "S5: modifying the source file"
	write_pdf_doc "hirame render probe v2 kaitei"

	log "S5: waiting for re-ingestion (a new content version)"
	_n=180
	_new_version=$_old_version
	while [ "$_n" -gt 0 ]; do
		_new_version=$(psql_q "select cv.content_hash from documents d
		                       join content_versions cv on cv.id = d.current_content_version_id
		                       where d.id = $_doc_id")
		[ "$_new_version" != "$_old_version" ] && break
		_n=$((_n - 5))
		sleep 5
	done
	check_true 'the content version changed' test "$_new_version" != "$_old_version"
	printf '  new content version %s\n' "$_new_version"

	log "S5: the old thumbnail is gone"
	# The invalidation job is enqueued by the reconciler and run by River, so
	# it lands shortly after the new version does rather than with it.
	_n=60
	_rows=unknown
	while [ "$_n" -gt 0 ]; do
		_rows=$(psql_q "select count(*) from thumbnail_cache tc
		                join content_versions cv on cv.id = tc.content_version_id
		                where cv.content_hash = '$_old_version'")
		[ "$_rows" = 0 ] && break
		_n=$((_n - 5))
		sleep 5
	done
	check_eq 'thumbnail_cache rows for the old version' 0 "$_rows"
	check_eq 'objects left in the bucket' 0 "$(object_count)"

	log "S5: the old ref no longer resolves"
	_req=$(printf '{"ref":%s,"pageNumber":1,"spec":{"format":"IMAGE_FORMAT_PNG"}}' "$_old_ref")
	_code=$(rpc_status "$API" hirame.v1.ThumbnailService/GetThumbnail "$_req")
	_body=$(cat "$RUN/rpc.out")
	printf '  HTTP %s %s\n' "$_code" "$_body"
	# Connect's JSON codec maps FAILED_PRECONDITION to HTTP 412 with the code
	# in the body; the body is what the contract specifies, so assert on it.
	check_eq 'old-ref error code' failed_precondition \
		"$(printf '%s' "$_body" | jq -r '.code // empty')"

	log "S5: the new version is searchable with the updated text"
	check_true 'updated text is searchable' wait_for_hit 120 'kaitei'
	_new_hit=$(search_hit 'kaitei')
	check_eq 'the hit names the new content version' "$_new_version" \
		"$(printf '%s' "$_new_hit" | jq -r '.ref.contentVersionId')"

	log "S5: a thumbnail for the new version renders and caches"
	_new_req=$(printf '{"ref":%s,"pageNumber":1,"spec":{"format":"IMAGE_FORMAT_PNG"}}' \
		"$(printf '%s' "$_new_hit" | jq -c '.ref')")
	check_eq 'new thumbnail HTTP status' 200 \
		"$(rpc_status "$API" hirame.v1.ThumbnailService/GetThumbnail "$_new_req")"
	check_eq 'exactly one object again' 1 "$(object_count)"
	return $SCENARIO_FAILED
}
