import { QueryClientProvider } from "@tanstack/preact-query";
import { ErrorBoundary, LocationProvider, Route, Router } from "preact-iso";

import { browserClients } from "./api/clients";
import { ApiProvider } from "./api/provider";
import { createQueryClient } from "./api/query-client";
import { ThemeToggle } from "./components/theme-toggle";
import { DocumentPage } from "./routes/document";
import { SearchPage } from "./routes/search";

const queryClient = createQueryClient();

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <ApiProvider clients={browserClients}>
        <LocationProvider>
          <div class="min-h-dvh bg-base-100 text-base-content">
            <header class="navbar bg-base-200 px-4">
              <a class="flex-1 text-lg font-semibold" href="/">
                hirame
              </a>
              <ThemeToggle />
            </header>
            <main class="mx-auto w-full max-w-3xl p-4">
              <ErrorBoundary>
                <Router>
                  <Route path="/" component={SearchPage} />
                  <Route path="/doc/:id" component={DocumentPage} />
                  <Route default component={NotFound} />
                </Router>
              </ErrorBoundary>
            </main>
          </div>
        </LocationProvider>
      </ApiProvider>
    </QueryClientProvider>
  );
}

function NotFound() {
  return (
    <div role="alert" class="alert alert-warning">
      <span>ページが見つかりません</span>
    </div>
  );
}
