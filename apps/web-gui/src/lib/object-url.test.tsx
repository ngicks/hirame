import { render, waitFor } from "@testing-library/preact";
import { describe, expect, it, vi } from "vitest";

import { useObjectUrl } from "./object-url";

function Preview({ bytes }: { bytes: Uint8Array | undefined }) {
  const url = useObjectUrl(bytes, "image/png");
  return <img alt="preview" src={url ?? ""} />;
}

describe("useObjectUrl", () => {
  it("publishes a blob URL for the bytes", async () => {
    const { getByAltText } = render(<Preview bytes={new Uint8Array([1, 2])} />);

    await waitFor(() => {
      expect(getByAltText("preview").getAttribute("src")).toMatch(/^blob:/);
    });
  });

  it("revokes the URL on unmount", async () => {
    const revoke = vi.spyOn(URL, "revokeObjectURL");
    const { getByAltText, unmount } = render(
      <Preview bytes={new Uint8Array([1, 2])} />,
    );
    await waitFor(() => {
      expect(getByAltText("preview").getAttribute("src")).toMatch(/^blob:/);
    });
    const url = getByAltText("preview").getAttribute("src");

    unmount();

    expect(revoke).toHaveBeenCalledWith(url);
    revoke.mockRestore();
  });

  it("publishes nothing for empty or missing bytes", () => {
    const { getByAltText, rerender } = render(<Preview bytes={undefined} />);
    expect(getByAltText("preview").getAttribute("src")).toBe("");

    rerender(<Preview bytes={new Uint8Array()} />);
    expect(getByAltText("preview").getAttribute("src")).toBe("");
  });
});
