# S7 -- the deployment's only published surface answers.
#
# D-010: the browser talks to Caddy and nothing else, and the ConnectRPC API is
# same-origin behind it. Two requests are enough to prove both halves of
# apps/web-gui/Caddyfile: the static handler serving the SPA shell, and the
# /api handle proxying to search-api on loopback.

scenario_s7() {
	log "S7: the SPA shell"
	_shell=$(curl -sS "$WEB/")
	# Case-folded: the doctype is not case-sensitive and Vite emits it
	# lowercase, so asserting the canonical spelling would test the bundler.
	check_contains 'GET /' '<!doctype html>' "$(printf '%s' "$_shell" | tr 'A-Z' 'a-z')"
	check_contains 'GET /' '<div id="app">' "$_shell"

	# preact-iso routes client-side, so an unmatched path has to resolve to the
	# shell rather than to a 404. That is the try_files line in the Caddyfile.
	check_eq 'SPA fallback for a client-side route' 200 \
		"$(curl -sS -o /dev/null -w '%{http_code}' "$WEB/search?q=test")"

	log "S7: the API through the proxy"
	_code=$(rpc_status "$WEB" hirame.v1.SearchService/Search \
		'{"query":"四半期経営会議","pageSize":5,"maxSnippets":2}')
	check_eq 'proxied Search HTTP status' 200 "$_code"
	_hits=$(jq -r '.hits | length' "$RUN/rpc.out")
	printf '  proxied search returned %s hit(s): %s\n' "$_hits" \
		"$(jq -c '[.hits[].fileName]' "$RUN/rpc.out")"
	check_true 'proxied search returned a hit' test "$_hits" -ge 1
	return $SCENARIO_FAILED
}
