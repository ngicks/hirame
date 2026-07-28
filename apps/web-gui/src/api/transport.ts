import { createConnectTransport } from "@connectrpc/connect-web";

/**
 * Relative on purpose. The API is served same-origin behind the GUI's reverse
 * proxy (D-010), and vite.config.ts proxies the same prefix in development, so
 * the client never needs to know an API host.
 *
 * Must match server.APIPrefix in apps/search-api.
 */
export const API_BASE_URL = "/api";

export const transport = createConnectTransport({ baseUrl: API_BASE_URL });
