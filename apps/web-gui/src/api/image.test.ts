import { describe, expect, it } from "vitest";

import { ImageFormat } from "../gen/hirame/v1/render_pb";
import { imageMediaType } from "./image";

describe("imageMediaType", () => {
  it.each([
    [ImageFormat.PNG, "image/png"],
    [ImageFormat.JPEG, "image/jpeg"],
    [ImageFormat.WEBP, "image/webp"],
    [ImageFormat.AVIF, "image/avif"],
  ])("maps format %i to %s", (format, expected) => {
    expect(imageMediaType(format)).toBe(expected);
  });

  it("falls back to PNG for an unresolved format", () => {
    expect(imageMediaType(ImageFormat.UNSPECIFIED)).toBe("image/png");
  });
});
