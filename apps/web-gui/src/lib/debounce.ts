import { useSignal, useSignalEffect } from "@preact/signals";
import type { Signal } from "@preact/signals";

/**
 * Mirrors `source` once it has been quiet for `delayMs`.
 *
 * Writable rather than read-only so a caller can commit early — submitting the
 * form should not make the user wait out a delay meant for typing.
 */
export function useDebouncedSignal<T>(
  source: Signal<T>,
  delayMs: number,
): Signal<T> {
  const debounced = useSignal(source.peek());

  useSignalEffect(() => {
    const next = source.value;
    if (next === debounced.peek()) {
      return;
    }
    const timer = setTimeout(() => {
      debounced.value = next;
    }, delayMs);
    return () => clearTimeout(timer);
  });

  return debounced;
}
