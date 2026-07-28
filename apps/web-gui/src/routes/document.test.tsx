import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { ConnectRouter } from "@connectrpc/connect";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { fireEvent, screen, waitFor } from "@testing-library/preact";
import { Route, Router } from "preact-iso";
import { describe, expect, it } from "vitest";

import {
  DocumentService,
  ExtractionState,
} from "../gen/hirame/v1/document_pb";
import { ImageFormat, RenderService } from "../gen/hirame/v1/render_pb";
import { renderRoute } from "../test/harness";
import { DocumentPage } from "./document";

const MODIFIED = new Date("2026-06-15T12:00:00Z");
const FIRST_SEEN = new Date("2026-06-14T12:00:00Z");

const DOCUMENT = {
  documentId: "doc-1",
  mountpointId: "mp-1",
  relativePath: "2026/日本語/報告書.pdf",
  fileName: "報告書.pdf",
  deleted: false,
  discoveredTime: timestampFromDate(FIRST_SEEN),
  modifiedTime: timestampFromDate(MODIFIED),
  currentVersion: {
    contentVersionId: "v1",
    mediaType: "application/pdf",
    sizeBytes: 1536n,
    pageCount: 3,
    extractionState: ExtractionState.SUCCEEDED,
    hasExtractedText: true,
    firstSeenTime: timestampFromDate(FIRST_SEEN),
  },
};

function servesRenders(router: ConnectRouter, pages: number[] = []) {
  router.service(RenderService, {
    async *renderPage(request) {
      pages.push(request.pageNumber);
      yield {
        payload: {
          case: "info" as const,
          value: {
            ref: request.ref,
            pageNumber: request.pageNumber,
            format: ImageFormat.PNG,
            size: { width: 800, height: 1000 },
            totalBytes: 4n,
          },
        },
      };
      yield {
        payload: { case: "chunk" as const, value: new Uint8Array([1, 2, 3, 4]) },
      };
    },
  });
}

function NoMatch() {
  return <p>no route matched</p>;
}

function mount(transport: ReturnType<typeof createRouterTransport>, url: string) {
  return renderRoute(
    <Router>
      <Route path="/doc/:id" component={DocumentPage} />
      <Route default component={NoMatch} />
    </Router>,
    { transport, url },
  );
}

describe("DocumentPage", () => {
  it("renders metadata, version info and extraction availability", async () => {
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => ({ document: DOCUMENT }),
      });
      servesRenders(router);
    });

    mount(transport, "/doc/doc-1?v=v1");

    expect(await screen.findByText("報告書.pdf")).toBeTruthy();
    expect(screen.getByText("2026/日本語/報告書.pdf")).toBeTruthy();
    expect(screen.getByText("application/pdf")).toBeTruthy();
    expect(screen.getByText("1.5 KiB")).toBeTruthy();
    expect(screen.getByText("v1")).toBeTruthy();
    expect(screen.getByText("抽出済み")).toBeTruthy();
    expect(screen.getByText("本文あり")).toBeTruthy();
    expect(screen.getAllByText(/2026/).length).toBeGreaterThan(0);
  });

  it("renders the first page and navigates between pages", async () => {
    const pages: number[] = [];
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => ({ document: DOCUMENT }),
      });
      servesRenders(router, pages);
    });

    mount(transport, "/doc/doc-1?v=v1");

    const first = await screen.findByAltText("報告書.pdf の 1 ページ目");
    expect(first.getAttribute("src")).toMatch(/^blob:/);
    expect(screen.getByText("1 / 3")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "前のページ" }).hasAttribute("disabled"),
    ).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "次のページ" }));

    expect(await screen.findByAltText("報告書.pdf の 2 ページ目")).toBeTruthy();
    expect(screen.getByText("2 / 3")).toBeTruthy();
    expect(pages).toEqual([1, 2]);
  });

  it("hides the pager when the page count is unknown", async () => {
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => ({
          document: {
            ...DOCUMENT,
            currentVersion: { ...DOCUMENT.currentVersion, pageCount: 0 },
          },
        }),
      });
      servesRenders(router);
    });

    mount(transport, "/doc/doc-1");

    expect(await screen.findByAltText("報告書.pdf の 1 ページ目")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "次のページ" })).toBeNull();
  });

  it("notes that the document changed since the search result was opened", async () => {
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => ({ document: DOCUMENT }),
      });
      servesRenders(router);
    });

    mount(transport, "/doc/doc-1?v=v0");

    expect(
      await screen.findByText(
        "検索結果を取得したあとに文書が更新されました。最新の版を表示しています。",
      ),
    ).toBeTruthy();
  });

  it("offers a reload when a render is rejected as stale", async () => {
    let currentVersion = "v1";
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => ({
          document: {
            ...DOCUMENT,
            currentVersion: {
              ...DOCUMENT.currentVersion,
              contentVersionId: currentVersion,
            },
          },
        }),
      });
      router.service(RenderService, {
        async *renderPage(request) {
          if (request.ref?.contentVersionId !== "v2") {
            throw new ConnectError("superseded", Code.FailedPrecondition);
          }
          yield {
            payload: {
              case: "info" as const,
              value: {
                ref: request.ref,
                pageNumber: request.pageNumber,
                format: ImageFormat.PNG,
                size: { width: 800, height: 1000 },
                totalBytes: 4n,
              },
            },
          };
          yield {
            payload: {
              case: "chunk" as const,
              value: new Uint8Array([1, 2, 3, 4]),
            },
          };
        },
      });
    });

    mount(transport, "/doc/doc-1");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("文書が更新されています");

    currentVersion = "v2";
    fireEvent.click(screen.getByRole("button", { name: "最新の版を読み込む" }));

    expect(await screen.findByAltText("報告書.pdf の 1 ページ目")).toBeTruthy();
  });

  it("drops a render stream that answers for another content version", async () => {
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => ({ document: DOCUMENT }),
      });
      router.service(RenderService, {
        async *renderPage(request) {
          yield {
            payload: {
              case: "info" as const,
              value: {
                ref: { documentId: "doc-1", contentVersionId: "v-other" },
                pageNumber: request.pageNumber,
                format: ImageFormat.PNG,
                size: { width: 800, height: 1000 },
                totalBytes: 4n,
              },
            },
          };
          yield {
            payload: {
              case: "chunk" as const,
              value: new Uint8Array([1, 2, 3, 4]),
            },
          };
        },
      });
    });

    mount(transport, "/doc/doc-1");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("文書が更新されています");
    expect(screen.queryByAltText("報告書.pdf の 1 ページ目")).toBeNull();
  });

  it("reports an unknown document", async () => {
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => {
          throw new ConnectError("no such document", Code.NotFound);
        },
      });
      servesRenders(router);
    });

    mount(transport, "/doc/missing");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("文書が見つかりません");
  });

  it("explains a tombstoned document instead of rendering it", async () => {
    let rendered = false;
    const transport = createRouterTransport((router) => {
      router.service(DocumentService, {
        getDocument: () => ({
          document: { ...DOCUMENT, deleted: true, currentVersion: undefined },
        }),
      });
      router.service(RenderService, {
        async *renderPage() {
          rendered = true;
          yield { payload: { case: undefined } };
        },
      });
    });

    mount(transport, "/doc/doc-1");

    expect(
      await screen.findByText(
        "この文書は削除されています。表示できる内容はありません。",
      ),
    ).toBeTruthy();
    await waitFor(() => {
      expect(screen.queryByLabelText("ページを描画中")).toBeNull();
    });
    expect(rendered).toBe(false);
  });
});
