# S4 -- a thumbnail round-trips into VersityGW and is served back from cache.
#
# Two identical requests. The first must render through Gahaku and store an
# object; the second must not render again. The evidence is the accounting row
# and the object count, both of which stay at one, plus the fact that the bytes
# come back as a valid image.

# Set by S4 and read by S5, which needs the ref that produced the cached object.
S4_REF=

scenario_s4() {
	_hit=$(search_hit 'render probe')
	S4_REF=$(printf '%s' "$_hit" | jq -c '.ref')
	printf '  ref: %s\n' "$S4_REF"
	check_true 'the PDF hit carries a ref' test -n "$S4_REF"

	_req=$(printf '{"ref":%s,"pageNumber":1,"spec":{"format":"IMAGE_FORMAT_PNG"}}' "$S4_REF")

	log "S4: first request (renders through gahaku, stores in versitygw)"
	check_eq 'objects before' 0 "$(object_count)"
	_t0=$(date +%s%N)
	_code=$(rpc_status "$API" hirame.v1.ThumbnailService/GetThumbnail "$_req")
	_ms1=$((($(date +%s%N) - _t0) / 1000000))
	check_eq 'first request HTTP status' 200 "$_code"
	[ "$_code" = 200 ] || {
		sed 's/^/  | /' "$RUN/rpc.out"
		return 1
	}
	cp "$RUN/rpc.out" "$RUN/thumb1.json"
	printf '  first request: %sms\n' "$_ms1"

	# PNG's 8-byte signature. Decoding the base64 the JSON carries and
	# checking the magic is the difference between "the RPC returned bytes"
	# and "the renderer produced an image".
	jq -r '.image' "$RUN/thumb1.json" | base64 -d >"$RUN/thumb1.png"
	check_eq 'PNG magic' '89504e470d0a1a0a' \
		"$(head -c 8 "$RUN/thumb1.png" | od -An -tx1 | tr -d ' \n')"
	check_true 'image is not empty' test -s "$RUN/thumb1.png"
	printf '  image: %s bytes, %s\n' "$(wc -c <"$RUN/thumb1.png" | tr -d ' ')" \
		"$(jq -c '{format,size}' "$RUN/thumb1.json")"

	log "S4: cache accounting and stored objects"
	check_eq 'thumbnail_cache rows' 1 "$(psql_q 'select count(*) from thumbnail_cache')"
	check_eq 'objects stored' 1 "$(object_count)"
	psql_q "select page, width, height, format, object_key, size_bytes from thumbnail_cache" |
		sed 's/^/  /'

	log "S4: second, identical request (must hit the cache)"
	_t0=$(date +%s%N)
	_code=$(rpc_status "$API" hirame.v1.ThumbnailService/GetThumbnail "$_req")
	_ms2=$((($(date +%s%N) - _t0) / 1000000))
	check_eq 'second request HTTP status' 200 "$_code"
	cp "$RUN/rpc.out" "$RUN/thumb2.json"
	printf '  second request: %sms (first was %sms)\n' "$_ms2" "$_ms1"

	# The cache is proved by the accounting and by the bytes being identical,
	# not by the timing: timing is reported because it is the symptom an
	# operator sees, but a slow machine must not fail the scenario.
	check_eq 'no second row' 1 "$(psql_q 'select count(*) from thumbnail_cache')"
	check_eq 'no second object' 1 "$(object_count)"
	check_true 'identical bytes returned' \
		cmp -s "$RUN/thumb1.json" "$RUN/thumb2.json"
	if [ "$_ms2" -lt "$_ms1" ]; then
		ok "cached request was faster ($_ms2 < $_ms1 ms)"
	else
		warn "cached request was not faster ($_ms2 >= $_ms1 ms); accounting still shows one render"
	fi
	return $SCENARIO_FAILED
}
