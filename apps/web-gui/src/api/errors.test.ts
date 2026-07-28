import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { describeFailure, failureKind, isRetriableFailure } from "./errors";

describe("failureKind", () => {
  it.each([
    [Code.NotFound, "notFound"],
    [Code.FailedPrecondition, "staleVersion"],
    [Code.OutOfRange, "outOfRange"],
    [Code.InvalidArgument, "invalidRequest"],
    [Code.PermissionDenied, "forbidden"],
    [Code.Unauthenticated, "forbidden"],
    [Code.Unavailable, "unavailable"],
    [Code.DeadlineExceeded, "unavailable"],
    [Code.Canceled, "canceled"],
    [Code.Internal, "unknown"],
  ])("maps code %i to %s", (code, expected) => {
    expect(failureKind(new ConnectError("boom", code))).toBe(expected);
  });

  it("treats a plain error as unknown", () => {
    expect(failureKind(new Error("boom"))).toBe("unknown");
  });
});

describe("describeFailure", () => {
  it("returns a user-facing message instead of the wire message", () => {
    const failure = describeFailure(
      new ConnectError("[failed_precondition] stale ref", Code.FailedPrecondition),
    );

    expect(failure.kind).toBe("staleVersion");
    expect(failure.message).toBe("文書が更新されています");
    expect(failure.message).not.toContain("failed_precondition");
    expect(failure.hint).not.toBe("");
  });
});

describe("isRetriableFailure", () => {
  it("retries only transport trouble", () => {
    expect(isRetriableFailure(new ConnectError("", Code.Unavailable))).toBe(true);
    expect(isRetriableFailure(new ConnectError("", Code.NotFound))).toBe(false);
    expect(
      isRetriableFailure(new ConnectError("", Code.FailedPrecondition)),
    ).toBe(false);
  });
});
