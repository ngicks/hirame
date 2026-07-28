import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Timestamp } from "@bufbuild/protobuf/wkt";

import { ExtractionState } from "../gen/hirame/v1/document_pb";

export const UNKNOWN_VALUE = "—";

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB"] as const;

export function formatBytes(bytes: bigint): string {
  let value = Number(bytes);
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = unit === 0 || value >= 100 ? 0 : 1;
  return `${value.toFixed(digits)} ${UNITS[unit]}`;
}

export function formatTimestamp(timestamp: Timestamp | undefined): string {
  if (timestamp === undefined) {
    return UNKNOWN_VALUE;
  }
  return timestampDate(timestamp).toLocaleString("ja-JP");
}

export function formatCount(count: bigint): string {
  return Number(count).toLocaleString("ja-JP");
}

export function extractionStateLabel(state: ExtractionState): string {
  switch (state) {
    case ExtractionState.PENDING:
      return "抽出待ち";
    case ExtractionState.SUCCEEDED:
      return "抽出済み";
    case ExtractionState.FAILED:
      return "抽出失敗";
    case ExtractionState.UNSUPPORTED:
      return "対象外の形式";
    case ExtractionState.UNSPECIFIED:
      return UNKNOWN_VALUE;
  }
}
