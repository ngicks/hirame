import { QueryClient } from "@tanstack/preact-query";

import { isRetriableFailure } from "./errors";

const MAX_RETRIES = 2;

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // NOT_FOUND, FAILED_PRECONDITION and INVALID_ARGUMENT are settled
        // answers; retrying them only multiplies the same failure.
        retry: (failureCount, error) =>
          failureCount < MAX_RETRIES && isRetriableFailure(error),
        refetchOnWindowFocus: false,
      },
    },
  });
}
