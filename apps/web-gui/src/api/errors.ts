import { Code, ConnectError } from "@connectrpc/connect";

/**
 * The user-facing states a failed call collapses into. The wire code is not
 * shown: it says nothing a user can act on, and Connect's default message
 * leaks internals such as the RPC path.
 */
export type FailureKind =
  | "notFound"
  | "staleVersion"
  | "outOfRange"
  | "invalidRequest"
  | "forbidden"
  | "unavailable"
  | "canceled"
  | "unknown";

export interface ApiFailure {
  kind: FailureKind;
  message: string;
  /** What to do next; empty when there is nothing useful to suggest. */
  hint: string;
}

export function failureCode(error: unknown): Code {
  return ConnectError.from(error).code;
}

export function failureKind(error: unknown): FailureKind {
  switch (failureCode(error)) {
    case Code.NotFound:
      return "notFound";
    case Code.FailedPrecondition:
      return "staleVersion";
    case Code.OutOfRange:
      return "outOfRange";
    case Code.InvalidArgument:
      return "invalidRequest";
    case Code.PermissionDenied:
    case Code.Unauthenticated:
      return "forbidden";
    case Code.Unavailable:
    case Code.DeadlineExceeded:
    case Code.ResourceExhausted:
      return "unavailable";
    case Code.Canceled:
      return "canceled";
    default:
      return "unknown";
  }
}

const FAILURES: Record<FailureKind, Omit<ApiFailure, "kind">> = {
  notFound: {
    message: "文書が見つかりません",
    hint: "削除されたか、まだ登録されていない可能性があります。",
  },
  staleVersion: {
    message: "文書が更新されています",
    hint: "最新の版を読み込み直してください。",
  },
  outOfRange: {
    message: "指定されたページはありません",
    hint: "ページ番号を確認してください。",
  },
  invalidRequest: {
    message: "リクエストを処理できませんでした",
    hint: "検索条件を変更して、もう一度お試しください。",
  },
  forbidden: {
    message: "この操作は許可されていません",
    hint: "",
  },
  unavailable: {
    message: "サーバーに接続できません",
    hint: "時間をおいて再試行してください。",
  },
  canceled: {
    message: "リクエストが中断されました",
    hint: "",
  },
  unknown: {
    message: "予期しないエラーが発生しました",
    hint: "時間をおいて再試行してください。",
  },
};

export function describeFailure(error: unknown): ApiFailure {
  const kind = failureKind(error);
  return { kind, ...FAILURES[kind] };
}

/** Only transport-level trouble is worth a second attempt; the rest is settled. */
export function isRetriableFailure(error: unknown): boolean {
  return failureKind(error) === "unavailable";
}
