import { useEffect, useState } from "preact/hooks";

/**
 * Holds a Blob URL for `bytes` and revokes it when the bytes change or the
 * component unmounts.
 *
 * The URL is created here rather than in a queryFn on purpose: query results
 * outlive the components that read them, so a URL minted during fetching would
 * be revoked while another mount still pointed an <img> at it.
 */
export function useObjectUrl(
  bytes: Uint8Array | undefined,
  mediaType: string,
): string | undefined {
  const [url, setUrl] = useState<string | undefined>(undefined);

  useEffect(() => {
    if (bytes === undefined || bytes.length === 0) {
      setUrl(undefined);
      return;
    }
    // Uint8Array is typed over ArrayBufferLike, which BlobPart rejects because
    // it admits SharedArrayBuffer. Decoded protobuf bytes are never shared,
    // and copying the buffer just to restate that would double every render.
    const part = bytes as BlobPart;
    const objectUrl = URL.createObjectURL(new Blob([part], { type: mediaType }));
    setUrl(objectUrl);
    return () => {
      URL.revokeObjectURL(objectUrl);
    };
  }, [bytes, mediaType]);

  return url;
}
