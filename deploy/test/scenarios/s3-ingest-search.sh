# S3 -- a file appearing under the watched mountpoint becomes searchable.
#
# The whole pipeline, none of it stubbed: the file lands on the watched tmpfs,
# the stand-in daemon's Scan reports it, the indexer records and enqueues it,
# a River worker sends it to Tika, the extracted text is NFKC-normalized and
# indexed by pg_search, and the ConnectRPC endpoint answers a Japanese query.
#
# The PDF fixture is seeded here too, so S4 and S5 have an ingested document
# Gahaku can actually render.

scenario_s3() {
	log "S3: seeding the watched mountpoint"
	write_japanese_doc v1
	write_pdf_doc "hirame render probe v1"
	ls -l "$DOCS" | sed 's/^/  /'

	log "S3: waiting for the Japanese document to become searchable"
	_t0=$(date +%s)
	check_true 'compound-noun query returns a hit' wait_for_hit 180 '四半期経営会議'
	printf '  ingest-to-searchable: %ss\n' "$(($(date +%s) - _t0))"

	_hit=$(search_hit '四半期経営会議')
	printf '  hit: %s\n' "$(printf '%s' "$_hit" | jq -c '{fileName,relativePath,mediaType,score}')"
	check_eq 'file name' japanese-report.txt "$(printf '%s' "$_hit" | jq -r '.fileName')"
	check_eq 'media type' text/plain "$(printf '%s' "$_hit" | jq -r '.mediaType')"

	# The API contract's snippets, not just a document id: a hit with no
	# highlighted segment would render as an unmarked block of text in the GUI.
	_hl=$(printf '%s' "$_hit" |
		jq -r '[.snippets[].segments[] | select(.highlighted == true) | .text] | join("|")')
	printf '  highlighted segments: %s\n' "$_hl"
	check_true 'at least one highlighted segment' test -n "$_hl"

	# NFKC normalization (D-012): the document holds half-width katakana, the
	# query is full-width, and only normalized indexing makes them meet.
	_hankaku=$(search_hit 'ハンカク')
	check_true 'half-width to full-width query matches' test -n "$_hankaku"

	# The PDF has to be ingested as well, otherwise S4 has nothing to render.
	check_true 'the PDF fixture is searchable' wait_for_hit 180 'render probe'
	_pdf=$(search_hit 'render probe')
	check_eq 'PDF media type' application/pdf "$(printf '%s' "$_pdf" | jq -r '.mediaType')"

	log "S3: what the database recorded"
	psql_q "select d.path, cv.content_hash, ec.content_type, ec.status,
	               length(ec.text_normalized)
	        from documents d
	        join content_versions cv on cv.id = d.current_content_version_id
	        left join extracted_contents ec on ec.content_version_id = cv.id
	        order by d.path" | sed 's/^/  /'
	return $SCENARIO_FAILED
}
