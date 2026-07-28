import type { Transport } from "@connectrpc/connect";
import { QueryClient, QueryClientProvider } from "@tanstack/preact-query";
import { render } from "@testing-library/preact";
import type { ComponentChild } from "preact";
import { LocationProvider } from "preact-iso";

import { createClients } from "../api/clients";
import { ApiProvider } from "../api/provider";

/**
 * Mounts a route the way the application does — router transport, query client
 * and location provider — so nothing under test needs a fetch stub. Retries are
 * off because a retried failure would only make assertions wait for the same
 * outcome.
 */
export function renderRoute(
  ui: ComponentChild,
  options: {
    transport: Transport;
    url?: string;
    /** Pass one to keep a cache alive across mounts. */
    queryClient?: QueryClient;
  },
) {
  if (options.url !== undefined) {
    window.history.replaceState(null, "", options.url);
  }

  const queryClient =
    options.queryClient ??
    new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 } },
    });

  return render(
    <QueryClientProvider client={queryClient}>
      <ApiProvider clients={createClients(options.transport)}>
        <LocationProvider>{ui}</LocationProvider>
      </ApiProvider>
    </QueryClientProvider>,
  );
}
