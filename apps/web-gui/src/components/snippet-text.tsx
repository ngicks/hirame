import type { Snippet } from "../gen/hirame/v1/search_pb";

/**
 * Renders one excerpt from its segments. Segment order is the excerpt, so
 * nothing here re-slices the text: the server already split it at the
 * highlight boundaries precisely so a byte offset never has to be translated
 * into a UTF-16 index.
 */
export function SnippetText({ snippet }: { snippet: Snippet }) {
  return (
    <p class="text-sm leading-relaxed break-all text-base-content/80">
      {snippet.segments.map((segment, index) =>
        segment.highlighted ? (
          <mark
            key={index}
            class="rounded-sm bg-accent px-0.5 text-accent-content"
          >
            {segment.text}
          </mark>
        ) : (
          <span key={index}>{segment.text}</span>
        ),
      )}
    </p>
  );
}
