import { createContext } from "preact";
import type { ComponentChildren } from "preact";
import { useContext } from "preact/hooks";

import type { ApiClients } from "./clients";

const ApiContext = createContext<ApiClients | null>(null);

export function ApiProvider(props: {
  clients: ApiClients;
  children: ComponentChildren;
}) {
  return (
    <ApiContext.Provider value={props.clients}>
      {props.children}
    </ApiContext.Provider>
  );
}

export function useApi(): ApiClients {
  const clients = useContext(ApiContext);
  if (clients === null) {
    throw new Error("useApi requires an <ApiProvider> ancestor");
  }
  return clients;
}
