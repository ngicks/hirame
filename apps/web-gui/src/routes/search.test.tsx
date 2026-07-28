import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { ConnectRouter } from "@connectrpc/connect";
import { QueryClient } from "@tanstack/preact-query";
import { fireEvent, screen, waitFor } from "@testing-library/preact";
import { describe, expect, it } from "vitest";

import { DocumentService } from "../gen/hirame/v1/document_pb";
import { ImageFormat } from "../gen/hirame/v1/render_pb";
import { SearchService } from "../gen/hirame/v1/search_pb";
import { ThumbnailService } from "../gen/hirame/v1/thumbnail_pb";
import { renderRoute } from "../test/harness";
import { SearchPage } from "./search";

const REPORT = {
  ref: { documentId: "doc-1", contentVersionId: "v1" },
  fileName: "報告書.pdf",
  relativePath: "2026/日本語/報告書.pdf",
  mediaType: "application/pdf",
  score: 12.3456,
  snippets: [
    {
      segments: [
        { text: "これは", highlighted: false },
        { text: "日本語", highlighted: true },
        { text: "の文書です。", highlighted: false },
      ],
    },
  ],
};

const MEMO = {
  ref: { documentId: "doc-2", contentVersionId: "v9" },
  fileName: "議事録.txt",
  relativePath: "2026/議事録.txt",
  mediaType: "text/plain",
  score: 3.5,
  snippets: [],
};

const IMAGE = new Uint8Array([137, 80, 78, 71]);

function servesThumbnails(router: ConnectRouter) {
  router.service(ThumbnailService, {
    getThumbnail: (request) => ({
      ref: request.ref,
      pageNumber: request.pageNumber,
      format: ImageFormat.WEBP,
      size: { width: 192, height: 192 },
      image: IMAGE,
    }),
  });
}

function typeQuery(text: string) {
  fireEvent.input(screen.getByLabelText("検索"), { target: { value: text } });
}

describe("SearchPage", () => {
  it("waits for a query before calling the API", () => {
    const queries: string[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: (request) => {
          queries.push(request.query);
          return { hits: [], nextPageToken: "", estimatedTotalHits: 0n };
        },
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });

    expect(screen.getByText("キーワードを入力すると検索を開始します。")).toBeTruthy();
    expect(queries).toEqual([]);
  });

  it("debounces typing into a single request", async () => {
    const queries: string[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: (request) => {
          queries.push(request.query);
          return { hits: [REPORT], nextPageToken: "", estimatedTotalHits: 1n };
        },
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日");
    typeQuery("日本");
    typeQuery("日本語");

    expect(await screen.findByText("報告書.pdf")).toBeTruthy();
    expect(queries).toEqual(["日本語"]);
  });

  it("renders title, path and score for each hit", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => ({
          hits: [REPORT, MEMO],
          nextPageToken: "",
          estimatedTotalHits: 2n,
        }),
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日本語");

    const title = await screen.findByText("報告書.pdf");
    expect(title.getAttribute("href")).toBe(
      "/doc/doc-1?v=v1",
    );
    expect(screen.getByText("2026/日本語/報告書.pdf")).toBeTruthy();
    expect(screen.getByText("BM25 12.35")).toBeTruthy();
    expect(screen.getByText("議事録.txt")).toBeTruthy();
    expect(screen.getByText("約 2 件 / 1 ページ目")).toBeTruthy();
  });

  it("marks only the highlighted segments of a Japanese snippet", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => ({
          hits: [REPORT],
          nextPageToken: "",
          estimatedTotalHits: 1n,
        }),
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日本語");

    const highlighted = await screen.findByText("日本語");
    expect(highlighted.tagName).toBe("MARK");
    expect(highlighted.parentElement?.textContent).toBe("これは日本語の文書です。");
    expect(screen.getByText("これは").tagName).toBe("SPAN");
    expect(screen.getByText("の文書です。").tagName).toBe("SPAN");
  });

  it("shows a thumbnail fetched for the hit's ref", async () => {
    const refs: string[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => ({
          hits: [REPORT],
          nextPageToken: "",
          estimatedTotalHits: 1n,
        }),
      });
      router.service(ThumbnailService, {
        getThumbnail: (request) => {
          refs.push(request.ref?.contentVersionId ?? "");
          return {
            ref: request.ref,
            pageNumber: request.pageNumber,
            format: ImageFormat.WEBP,
            size: { width: 192, height: 192 },
            image: IMAGE,
          };
        },
      });
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日本語");

    const image = await screen.findByAltText("報告書.pdf のプレビュー");
    expect(image.getAttribute("src")).toMatch(/^blob:/);
    expect(refs).toEqual(["v1"]);
  });

  it("re-reads the document ref when a thumbnail is rejected as stale", async () => {
    const refs: string[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => ({
          hits: [REPORT],
          nextPageToken: "",
          estimatedTotalHits: 1n,
        }),
      });
      router.service(DocumentService, {
        getDocument: () => ({
          document: {
            documentId: "doc-1",
            mountpointId: "mp-1",
            relativePath: "2026/日本語/報告書.pdf",
            fileName: "報告書.pdf",
            deleted: false,
            currentVersion: { contentVersionId: "v2", pageCount: 1 },
          },
        }),
      });
      router.service(ThumbnailService, {
        getThumbnail: (request) => {
          const version = request.ref?.contentVersionId ?? "";
          refs.push(version);
          if (version !== "v2") {
            throw new ConnectError("superseded", Code.FailedPrecondition);
          }
          return {
            ref: request.ref,
            pageNumber: request.pageNumber,
            format: ImageFormat.WEBP,
            size: { width: 192, height: 192 },
            image: IMAGE,
          };
        },
      });
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日本語");

    const image = await screen.findByAltText("報告書.pdf のプレビュー");
    expect(image.getAttribute("src")).toMatch(/^blob:/);
    expect(refs).toEqual(["v1", "v2"]);
  });

  it("keeps a thumbnail that answers for the ref it was asked for", async () => {
    const refs: string[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => ({
          hits: [REPORT],
          nextPageToken: "",
          estimatedTotalHits: 1n,
        }),
      });
      router.service(ThumbnailService, {
        getThumbnail: (request) => {
          refs.push(request.ref?.contentVersionId ?? "");
          return {
            ref: request.ref,
            pageNumber: request.pageNumber,
            format: ImageFormat.WEBP,
            size: { width: 192, height: 192 },
            image: IMAGE,
          };
        },
      });
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    const first = renderRoute(<SearchPage />, {
      transport,
      url: "/",
      queryClient,
    });
    typeQuery("日本語");
    await screen.findByAltText("報告書.pdf のプレビュー");
    first.unmount();

    renderRoute(<SearchPage />, { transport, url: "/", queryClient });
    typeQuery("日本語");
    await screen.findByAltText("報告書.pdf のプレビュー");

    expect(refs).toEqual(["v1"]);
  });

  it("does not keep a recovered thumbnail under the ref it was asked for", async () => {
    const refs: string[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => ({
          hits: [REPORT],
          nextPageToken: "",
          estimatedTotalHits: 1n,
        }),
      });
      router.service(DocumentService, {
        getDocument: () => ({
          document: {
            documentId: "doc-1",
            fileName: "報告書.pdf",
            deleted: false,
            currentVersion: { contentVersionId: "v2", pageCount: 1 },
          },
        }),
      });
      router.service(ThumbnailService, {
        getThumbnail: (request) => {
          const version = request.ref?.contentVersionId ?? "";
          refs.push(version);
          if (version !== "v2") {
            throw new ConnectError("superseded", Code.FailedPrecondition);
          }
          return {
            ref: request.ref,
            pageNumber: request.pageNumber,
            format: ImageFormat.WEBP,
            size: { width: 192, height: 192 },
            image: IMAGE,
          };
        },
      });
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    const first = renderRoute(<SearchPage />, {
      transport,
      url: "/",
      queryClient,
    });
    typeQuery("日本語");
    await screen.findByAltText("報告書.pdf のプレビュー");
    first.unmount();

    renderRoute(<SearchPage />, { transport, url: "/", queryClient });
    typeQuery("日本語");
    await screen.findByAltText("報告書.pdf のプレビュー");

    // The v1 entry holds a v2 image, so it is revalidated rather than reused:
    // the document could have moved on again since it was recovered.
    await waitFor(() => {
      expect(refs).toEqual(["v1", "v2", "v1", "v2"]);
    });
  });

  it("pages forward and back with the tokens the server hands out", async () => {
    const tokens: string[] = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: (request) => {
          tokens.push(request.pageToken);
          return request.pageToken === "page-2"
            ? { hits: [MEMO], nextPageToken: "", estimatedTotalHits: 2n }
            : { hits: [REPORT], nextPageToken: "page-2", estimatedTotalHits: 2n };
        },
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日本語");

    await screen.findByText("報告書.pdf");
    expect(screen.getByRole("button", { name: "前へ" }).hasAttribute("disabled")).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "次へ" }));

    expect(await screen.findByText("議事録.txt")).toBeTruthy();
    expect(screen.getByText("約 2 件 / 2 ページ目")).toBeTruthy();
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: "次へ" }).hasAttribute("disabled"),
      ).toBe(true);
    });

    fireEvent.click(screen.getByRole("button", { name: "前へ" }));

    expect(await screen.findByText("報告書.pdf")).toBeTruthy();
    // Going back reuses the tokenless first page rather than inventing a
    // backward cursor, which the contract does not provide.
    expect(tokens.slice(0, 2)).toEqual(["", "page-2"]);
    expect(tokens.every((token) => token === "" || token === "page-2")).toBe(true);
  });

  it("resets paging when the query changes", async () => {
    const requests: Array<{ query: string; pageToken: string }> = [];
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: (request) => {
          requests.push({ query: request.query, pageToken: request.pageToken });
          return request.pageToken === "page-2"
            ? { hits: [MEMO], nextPageToken: "", estimatedTotalHits: 2n }
            : { hits: [REPORT], nextPageToken: "page-2", estimatedTotalHits: 2n };
        },
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日本語");
    await screen.findByText("報告書.pdf");
    fireEvent.click(screen.getByRole("button", { name: "次へ" }));
    await screen.findByText("議事録.txt");

    typeQuery("会議");

    await waitFor(() => {
      expect(requests.at(-1)).toEqual({ query: "会議", pageToken: "" });
    });
    expect(
      requests.every(
        (request) => request.pageToken === "" || request.query === "日本語",
      ),
    ).toBe(true);
  });

  it("reports an empty result set", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => ({ hits: [], nextPageToken: "", estimatedTotalHits: 0n }),
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("該当なし");

    expect(await screen.findByText("一致する文書はありません。")).toBeTruthy();
  });

  it("reports a transport failure without leaking the wire message", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SearchService, {
        search: () => {
          throw new ConnectError("dial tcp: connection refused", Code.Unavailable);
        },
      });
      servesThumbnails(router);
    });

    renderRoute(<SearchPage />, { transport, url: "/" });
    typeQuery("日本語");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("サーバーに接続できません");
    expect(alert.textContent).not.toContain("connection refused");
  });
});
