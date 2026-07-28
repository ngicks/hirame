import { Field } from "@ark-ui/react/field";
import { useSignal } from "@preact/signals";

import { useSearch } from "../api/hooks";
import { refHref } from "../api/refs";
import { FailureAlert } from "../components/failure-alert";
import { SnippetText } from "../components/snippet-text";
import { Thumbnail } from "../components/thumbnail";
import type { SearchHit } from "../gen/hirame/v1/search_pb";
import { useDebouncedSignal } from "../lib/debounce";
import { formatCount } from "../lib/format";

const DEBOUNCE_MS = 250;

/**
 * page_token is forward-only and encodes the query it belongs to, so paging is
 * a stack of visited tokens rather than numbered pages, and the stack is
 * stamped with its query: a token is never paired with a query it did not come
 * from, not even for the one render between a keystroke and an effect.
 */
interface Paging {
  query: string;
  tokens: readonly string[];
  index: number;
}

export function SearchPage() {
  const draft = useSignal("");
  const committed = useDebouncedSignal(draft, DEBOUNCE_MS);
  const paging = useSignal<Paging>({ query: "", tokens: [""], index: 0 });

  const query = committed.value.trim();
  const current: Paging =
    paging.value.query === query
      ? paging.value
      : { query, tokens: [""], index: 0 };
  const pageToken = current.tokens[current.index] ?? "";

  const results = useSearch(query, pageToken);
  const hits = results.data?.hits ?? [];
  const nextPageToken = results.data?.nextPageToken ?? "";

  const goNext = () => {
    if (nextPageToken === "") {
      return;
    }
    paging.value = {
      query,
      tokens: [...current.tokens.slice(0, current.index + 1), nextPageToken],
      index: current.index + 1,
    };
  };

  const goPrevious = () => {
    if (current.index === 0) {
      return;
    }
    paging.value = { ...current, index: current.index - 1 };
  };

  return (
    <section class="flex flex-col gap-6">
      <form
        class="flex items-end gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          committed.value = draft.value;
        }}
      >
        <Field.Root class="grow">
          <Field.Label class="label">検索</Field.Label>
          <Field.Input
            class="input input-bordered w-full"
            placeholder="全文検索"
            value={draft.value}
            onInput={(event) => {
              draft.value = event.currentTarget.value;
            }}
          />
        </Field.Root>
        <button type="submit" class="btn btn-primary">
          検索
        </button>
      </form>

      {query === "" ? (
        <p class="text-base-content/60">
          キーワードを入力すると検索を開始します。
        </p>
      ) : (
        <>
          {results.isFetching && (
            <progress
              class="progress progress-primary w-full"
              aria-label="検索中"
            />
          )}

          {results.isError && (
            <FailureAlert
              error={results.error}
              onRetry={() => void results.refetch()}
            />
          )}

          {results.isSuccess && hits.length === 0 && (
            <div role="status" class="alert alert-info">
              <span>一致する文書はありません。</span>
            </div>
          )}

          {hits.length > 0 && (
            <>
              <p role="status" class="text-sm text-base-content/60">
                約 {formatCount(results.data?.estimatedTotalHits ?? 0n)} 件 /{" "}
                {current.index + 1} ページ目
              </p>
              <ul class="flex flex-col gap-3">
                {hits.map((hit) => (
                  <HitCard
                    key={`${hit.ref?.documentId ?? ""}:${hit.ref?.contentVersionId ?? ""}`}
                    hit={hit}
                  />
                ))}
              </ul>
              <div class="join self-center">
                <button
                  type="button"
                  class="btn btn-sm join-item"
                  disabled={current.index === 0 || results.isFetching}
                  onClick={goPrevious}
                >
                  前へ
                </button>
                <button
                  type="button"
                  class="btn btn-sm join-item"
                  disabled={nextPageToken === "" || results.isFetching}
                  onClick={goNext}
                >
                  次へ
                </button>
              </div>
            </>
          )}
        </>
      )}
    </section>
  );
}

function HitCard({ hit }: { hit: SearchHit }) {
  return (
    <li class="card bg-base-200">
      <div class="card-body flex-row gap-4 p-4">
        <Thumbnail docRef={hit.ref} alt={`${hit.fileName} のプレビュー`} />
        <div class="flex min-w-0 grow flex-col gap-1">
          <a class="link link-hover font-semibold" href={refHref(hit.ref)}>
            {hit.fileName}
          </a>
          <p class="truncate text-xs text-base-content/60">
            {hit.relativePath}
          </p>
          <div class="flex flex-wrap items-center gap-2">
            {hit.mediaType !== "" && (
              <span class="badge badge-ghost badge-sm">{hit.mediaType}</span>
            )}
            <span
              class="badge badge-outline badge-sm"
              title="BM25 スコア。この検索結果の中でのみ比較できます。"
            >
              BM25 {hit.score.toFixed(2)}
            </span>
          </div>
          {hit.snippets.map((snippet, index) => (
            <SnippetText key={index} snippet={snippet} />
          ))}
        </div>
      </div>
    </li>
  );
}
