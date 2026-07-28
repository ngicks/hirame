import { ImageFormat } from "../gen/hirame/v1/render_pb";

/**
 * A Blob URL only displays when its type is right, and neither response
 * carries a media type — just the ImageFormat it was encoded as. UNSPECIFIED
 * is not expected back, since both responses echo a resolved format, and is
 * treated as PNG rather than left unhandled.
 */
export function imageMediaType(format: ImageFormat): string {
  switch (format) {
    case ImageFormat.JPEG:
      return "image/jpeg";
    case ImageFormat.WEBP:
      return "image/webp";
    case ImageFormat.AVIF:
      return "image/avif";
    case ImageFormat.PNG:
    case ImageFormat.UNSPECIFIED:
      return "image/png";
  }
}
