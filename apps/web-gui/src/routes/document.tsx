import { useSignal } from "@preact/signals";
import { useRoute } from "preact-iso";

import { describeFailure } from "../api/errors";
import { useDocument, useRenderedPage } from "../api/hooks";
import { currentRef } from "../api/refs";
import type { DocRef } from "../api/refs";
import { FailureAlert } from "../components/failure-alert";
import type { ContentVersion, Document } from "../gen/hirame/v1/document_pb";
import {
  UNKNOWN_VALUE,
  extractionStateLabel,
  formatBytes,
  formatTimestamp,
} from "../lib/format";
import { useObjectUrl } from "../lib/object-url";

export function DocumentPage() {
  const { params, query } = useRoute();
  const documentId = params["id"] ?? "";
  // The version a search hit was opened at. Only used to tell the user the
  // document moved on; the current version is always what gets rendered.
  const openedVersion = query["v"] ?? "";

  const detail = useDocument(documentId);
  const document = detail.data?.document;
  const version = document?.currentVersion;
  const ref = currentRef(document);

  return (
    <section class="flex flex-col gap-4">
      <h1 class="text-xl font-semibold break-all">
        {document?.fileName ?? documentId}
      </h1>

      {detail.isFetching && (
        <progress class="progress progress-primary w-full" aria-label="読み込み中" />
      )}

      {detail.isError && (
        <FailureAlert
          error={detail.error}
          onRetry={() => void detail.refetch()}
        />
      )}

      {document !== undefined && (
        <>
          {openedVersion !== "" &&
            version !== undefined &&
            openedVersion !== version.contentVersionId && (
              <div role="status" class="alert alert-info">
                <span>
                  検索結果を取得したあとに文書が更新されました。最新の版を表示しています。
                </span>
              </div>
            )}

          <Metadata document={document} version={version} />

          {document.deleted ? (
            <div role="status" class="alert alert-warning">
              <span>この文書は削除されています。表示できる内容はありません。</span>
            </div>
          ) : version === undefined ? (
            <div role="status" class="alert alert-warning">
              <span>表示できる版がまだありません。</span>
            </div>
          ) : (
            <PageViewer
              docRef={ref}
              pageCount={version.pageCount}
              fileName={document.fileName}
              onStale={() => void detail.refetch()}
            />
          )}
        </>
      )}

      <a class="link self-start" href="/">
        検索へ戻る
      </a>
    </section>
  );
}

function Metadata(props: {
  document: Document;
  version: ContentVersion | undefined;
}) {
  const { document, version } = props;

  return (
    <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
      <dt class="opacity-70">パス</dt>
      <dd class="font-mono break-all">{document.relativePath}</dd>
      <dt class="opacity-70">マウントポイント</dt>
      <dd class="font-mono break-all">{document.mountpointId}</dd>
      <dt class="opacity-70">更新日時</dt>
      <dd>{formatTimestamp(document.modifiedTime)}</dd>
      <dt class="opacity-70">検出日時</dt>
      <dd>{formatTimestamp(document.discoveredTime)}</dd>
      <dt class="opacity-70">種別</dt>
      <dd>{version?.mediaType || UNKNOWN_VALUE}</dd>
      <dt class="opacity-70">サイズ</dt>
      <dd>{version === undefined ? UNKNOWN_VALUE : formatBytes(version.sizeBytes)}</dd>
      <dt class="opacity-70">ページ数</dt>
      <dd>
        {version === undefined || version.pageCount === 0
          ? UNKNOWN_VALUE
          : version.pageCount}
      </dd>
      <dt class="opacity-70">版</dt>
      <dd class="font-mono break-all">
        {version?.contentVersionId || UNKNOWN_VALUE}
      </dd>
      <dt class="opacity-70">初回検出</dt>
      <dd>{formatTimestamp(version?.firstSeenTime)}</dd>
      <dt class="opacity-70">テキスト抽出</dt>
      <dd class="flex flex-wrap items-center gap-2">
        {version === undefined ? (
          UNKNOWN_VALUE
        ) : (
          <>
            <span class="badge badge-ghost badge-sm">
              {extractionStateLabel(version.extractionState)}
            </span>
            <span
              class={`badge badge-sm ${version.hasExtractedText ? "badge-success" : "badge-neutral"}`}
            >
              {version.hasExtractedText ? "本文あり" : "本文なし"}
            </span>
            {version.extractionError !== "" && (
              <span class="text-xs opacity-70 break-all">
                {version.extractionError}
              </span>
            )}
          </>
        )}
      </dd>
    </dl>
  );
}

function PageViewer(props: {
  docRef: DocRef | undefined;
  pageCount: number;
  fileName: string;
  onStale: () => void;
}) {
  // page_count is 0 when unknown or when the media type has no page structure;
  // page 1 is still renderable, so only the pager is withheld.
  const paged = props.pageCount > 1;
  const viewerKey = `${props.docRef?.documentId ?? ""}:${props.docRef?.contentVersionId ?? ""}`;
  const position = useSignal({ key: viewerKey, page: 1 });
  const page = position.value.key === viewerKey ? position.value.page : 1;

  const render = useRenderedPage(props.docRef, page);
  const url = useObjectUrl(render.data?.bytes, render.data?.mediaType ?? "");

  const goTo = (next: number) => {
    if (next < 1 || (props.pageCount > 0 && next > props.pageCount)) {
      return;
    }
    position.value = { key: viewerKey, page: next };
  };

  return (
    <div class="flex flex-col gap-3">
      {paged && (
        <div class="flex items-center justify-center gap-3">
          <div class="join">
            <button
              type="button"
              class="btn btn-sm join-item"
              disabled={page <= 1}
              onClick={() => goTo(page - 1)}
            >
              前のページ
            </button>
            <button
              type="button"
              class="btn btn-sm join-item"
              disabled={page >= props.pageCount}
              onClick={() => goTo(page + 1)}
            >
              次のページ
            </button>
          </div>
          <span class="text-sm tabular-nums" role="status">
            {page} / {props.pageCount}
          </span>
        </div>
      )}

      {render.isFetching && (
        <progress class="progress progress-primary w-full" aria-label="ページを描画中" />
      )}

      {render.isError &&
        // A superseded ref cannot be retried as-is: only re-reading the
        // document produces a ref the server will accept.
        (describeFailure(render.error).kind === "staleVersion" ? (
          <FailureAlert
            error={render.error}
            retryLabel="最新の版を読み込む"
            onRetry={props.onStale}
          />
        ) : (
          <FailureAlert
            error={render.error}
            onRetry={() => void render.refetch()}
          />
        ))}

      {url !== undefined && (
        <img
          class="w-full rounded-box border border-base-300 bg-base-200"
          src={url}
          alt={`${props.fileName} の ${page} ページ目`}
        />
      )}

      {render.isSuccess && render.data.bytes.length === 0 && (
        <div role="status" class="alert alert-warning">
          <span>このページには表示できる内容がありません。</span>
        </div>
      )}
    </div>
  );
}
