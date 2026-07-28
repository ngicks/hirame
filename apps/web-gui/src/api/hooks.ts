import { Code, ConnectError } from "@connectrpc/connect";
import {
  keepPreviousData,
  skipToken,
  useQuery,
} from "@tanstack/preact-query";

import { ImageFormat } from "../gen/hirame/v1/render_pb";
import type { RenderPageInfo } from "../gen/hirame/v1/render_pb";
import type { GetThumbnailResponse } from "../gen/hirame/v1/thumbnail_pb";
import type { ApiClients } from "./clients";
import { failureCode } from "./errors";
import { imageMediaType } from "./image";
import { queryKeys } from "./keys";
import { useApi } from "./provider";
import { currentRef } from "./refs";
import type { DocRef } from "./refs";

export const SEARCH_PAGE_SIZE = 20;
const MAX_SNIPPETS = 3;

const THUMBNAIL_FORMAT = ImageFormat.WEBP;
const THUMBNAIL_SIZE = { width: 192, height: 192 };

const RENDER_FORMAT = ImageFormat.PNG;
const RENDER_SIZE = { width: 1600, height: 1600 };

export function useSearch(query: string, pageToken: string) {
  const api = useApi();
  return useQuery({
    queryKey: queryKeys.search(query, pageToken),
    queryFn:
      query === ""
        ? skipToken
        : () =>
            api.search.search({
              query,
              pageSize: SEARCH_PAGE_SIZE,
              pageToken,
              maxSnippets: MAX_SNIPPETS,
            }),
    // Page turns swap the token, and so the key; without this the list would
    // empty out and the pager would jump between every page.
    placeholderData: keepPreviousData,
  });
}

export function useDocument(documentId: string) {
  const api = useApi();
  return useQuery({
    queryKey: queryKeys.document(documentId),
    queryFn:
      documentId === ""
        ? skipToken
        : () => api.document.getDocument({ documentId }),
  });
}

export function useThumbnail(ref: DocRef | undefined, pageNumber = 1) {
  const api = useApi();
  const target = ref;
  return useQuery({
    queryKey: queryKeys.thumbnail(target, pageNumber),
    queryFn:
      target === undefined
        ? skipToken
        : () => fetchThumbnail(api, target, pageNumber),
    // Bytes addressed by a content version never change, so a response that
    // echoes the requested ref stays fresh forever. A recovered one answers
    // for a newer version than its key names, and holding that would put this
    // cache right back in the business of serving an image no live ref
    // addresses once the document moves on again.
    staleTime: (query) =>
      query.state.data?.ref?.contentVersionId === target?.contentVersionId
        ? Number.POSITIVE_INFINITY
        : 0,
  });
}

export interface RenderedPage {
  info: RenderPageInfo | undefined;
  bytes: Uint8Array;
  mediaType: string;
}

export function useRenderedPage(ref: DocRef | undefined, pageNumber: number) {
  const api = useApi();
  const target = ref;
  return useQuery({
    queryKey: queryKeys.renderedPage(target, pageNumber),
    queryFn:
      target === undefined || pageNumber < 1
        ? skipToken
        : ({ signal }) => fetchRenderedPage(api, target, pageNumber, signal),
    staleTime: Number.POSITIVE_INFINITY,
  });
}

/**
 * A superseded ref is rejected rather than upgraded server-side, so the client
 * owns the recovery: re-read the document, then retry once against whatever
 * version it is on now. Exactly once — a document that keeps changing must not
 * turn a thumbnail into an unbounded chase.
 */
async function fetchThumbnail(
  api: ApiClients,
  ref: DocRef,
  pageNumber: number,
): Promise<GetThumbnailResponse> {
  const spec = { maxSize: THUMBNAIL_SIZE, format: THUMBNAIL_FORMAT };
  try {
    return await api.thumbnail.getThumbnail({ ref, pageNumber, spec });
  } catch (error) {
    if (failureCode(error) !== Code.FailedPrecondition) {
      throw error;
    }
    const { document } = await api.document.getDocument({
      documentId: ref.documentId,
    });
    const fresh = currentRef(document);
    if (fresh === undefined) {
      throw error;
    }
    return await api.thumbnail.getThumbnail({ ref: fresh, pageNumber, spec });
  }
}

async function fetchRenderedPage(
  api: ApiClients,
  ref: DocRef,
  pageNumber: number,
  signal: AbortSignal,
): Promise<RenderedPage> {
  let info: RenderPageInfo | undefined;
  const chunks: Uint8Array[] = [];

  const stream = api.render.renderPage(
    { ref, pageNumber, format: RENDER_FORMAT, maxSize: RENDER_SIZE },
    { signal },
  );
  for await (const frame of stream) {
    if (frame.payload.case === "info") {
      info = frame.payload.value;
    } else if (frame.payload.case === "chunk") {
      chunks.push(frame.payload.value);
    }
  }

  // RenderPageInfo echoes the ref so a stream belonging to another version can
  // be dropped instead of painted.
  if (info !== undefined && info.ref?.contentVersionId !== ref.contentVersionId) {
    throw new ConnectError(
      "render stream answered for another content version",
      Code.FailedPrecondition,
    );
  }

  return {
    info,
    bytes: concatChunks(chunks),
    mediaType: imageMediaType(info?.format ?? RENDER_FORMAT),
  };
}

function concatChunks(chunks: readonly Uint8Array[]): Uint8Array {
  const total = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
  const merged = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    merged.set(chunk, offset);
    offset += chunk.length;
  }
  return merged;
}
