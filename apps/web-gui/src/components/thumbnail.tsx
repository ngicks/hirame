import { useThumbnail } from "../api/hooks";
import { imageMediaType } from "../api/image";
import { describeFailure } from "../api/errors";
import type { DocRef } from "../api/refs";
import { ImageFormat } from "../gen/hirame/v1/render_pb";
import { useObjectUrl } from "../lib/object-url";

/**
 * A failed thumbnail is not an error state for the page around it: plenty of
 * indexed documents have no renderable page at all, so it degrades to a
 * placeholder instead of an alert.
 */
export function Thumbnail(props: { docRef: DocRef | undefined; alt: string }) {
  const thumbnail = useThumbnail(props.docRef);
  const response = thumbnail.data;
  const url = useObjectUrl(
    response?.image,
    imageMediaType(response?.format ?? ImageFormat.UNSPECIFIED),
  );

  const frame =
    "size-20 shrink-0 overflow-hidden rounded-box border border-base-300 bg-base-200";

  if (url !== undefined) {
    return (
      <img class={`${frame} object-cover`} src={url} alt={props.alt} width={80} height={80} />
    );
  }

  if (thumbnail.isError) {
    return (
      <div
        class={`${frame} flex items-center justify-center text-xs text-base-content/50`}
        title={describeFailure(thumbnail.error).message}
      >
        プレビューなし
      </div>
    );
  }

  return <div class={`${frame} skeleton`} aria-hidden="true" />;
}
