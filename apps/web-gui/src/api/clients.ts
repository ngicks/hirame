import { createClient } from "@connectrpc/connect";
import type { Client, Transport } from "@connectrpc/connect";

import { DocumentService } from "../gen/hirame/v1/document_pb";
import { RenderService } from "../gen/hirame/v1/render_pb";
import { SearchService } from "../gen/hirame/v1/search_pb";
import { ThumbnailService } from "../gen/hirame/v1/thumbnail_pb";
import { transport } from "./transport";

export interface ApiClients {
  search: Client<typeof SearchService>;
  document: Client<typeof DocumentService>;
  render: Client<typeof RenderService>;
  thumbnail: Client<typeof ThumbnailService>;
}

/**
 * Built per transport rather than once per module so a test can drive the whole
 * UI through an in-memory router transport.
 */
export function createClients(transport: Transport): ApiClients {
  return {
    search: createClient(SearchService, transport),
    document: createClient(DocumentService, transport),
    render: createClient(RenderService, transport),
    thumbnail: createClient(ThumbnailService, transport),
  };
}

export const browserClients = createClients(transport);
