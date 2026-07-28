import type { Document } from "../gen/hirame/v1/document_pb";

/**
 * The document_id + content_version_id pair every render is addressed by.
 *
 * Structural rather than the generated DocumentRef message: request init
 * shapes accept a plain object, so nothing in the UI has to construct
 * messages, and a SearchHit's ref assigns to it unchanged.
 */
export interface DocRef {
  documentId: string;
  contentVersionId: string;
}

/**
 * The ref a document can currently be rendered at, or undefined when it has
 * none: a tombstone keeps its row but has no content version to address.
 */
export function currentRef(document: Document | undefined): DocRef | undefined {
  const version = document?.currentVersion;
  if (document === undefined || document.deleted || version === undefined) {
    return undefined;
  }
  return {
    documentId: document.documentId,
    contentVersionId: version.contentVersionId,
  };
}

export function refHref(ref: DocRef | undefined): string {
  if (ref === undefined) {
    return "/";
  }
  const version = encodeURIComponent(ref.contentVersionId);
  return `/doc/${encodeURIComponent(ref.documentId)}?v=${version}`;
}
