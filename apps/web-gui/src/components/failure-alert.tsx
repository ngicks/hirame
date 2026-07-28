import { describeFailure } from "../api/errors";

export function FailureAlert(props: {
  error: unknown;
  onRetry?: () => void;
  retryLabel?: string;
}) {
  const failure = describeFailure(props.error);

  return (
    <div role="alert" class="alert alert-error">
      <div class="flex flex-col items-start">
        <span class="font-medium">{failure.message}</span>
        {failure.hint !== "" && (
          <span class="text-sm opacity-80">{failure.hint}</span>
        )}
      </div>
      {props.onRetry !== undefined && (
        <button type="button" class="btn btn-sm" onClick={props.onRetry}>
          {props.retryLabel ?? "再試行"}
        </button>
      )}
    </div>
  );
}
