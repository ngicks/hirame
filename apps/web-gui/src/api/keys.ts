import type { DocRef } from "./refs";

/**
 * Every cached entry is keyed by what identifies it on the wire. Renders and
 * thumbnails include content_version_id, so a content change cannot resolve to
 * an entry produced from older bytes; a search page includes its page_token,
 * which encodes the query the token belongs to.
 */
export const queryKeys = {
  search: (query: string, pageToken: string) =>
    ["search", query, pageToken] as const,
  document: (documentId: string) => ["document", documentId] as const,
  thumbnail: (ref: DocRef | undefined, pageNumber: number) =>
    [
      "thumbnail",
      ref?.documentId ?? "",
      ref?.contentVersionId ?? "",
      pageNumber,
    ] as const,
  renderedPage: (ref: DocRef | undefined, pageNumber: number) =>
    ["render", ref?.documentId ?? "", ref?.contentVersionId ?? "", pageNumber] as const,
};
