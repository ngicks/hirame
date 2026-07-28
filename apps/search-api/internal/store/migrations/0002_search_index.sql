-- ParadeDB pg_search BM25 index over the NFKC-normalized extracted text
-- (D-012). Verified against pg_search 0.24.3 on PostgreSQL 18.4.

CREATE EXTENSION IF NOT EXISTS pg_search;

-- Two tokenizations of one column:
--
--   * the un-aliased Lindera field carries the Japanese segmentation (IPADIC)
--     and is what BM25 relevance is meant to come from;
--   * the aliased 2-3 gram field exists for recall — short queries, unsegmented
--     runs, and terms Lindera's dictionary does not know. It is deliberately
--     noisier, and callers OR the two rather than replacing one with the other.
--
-- Aliasing the ngram field rather than the Lindera field keeps
-- `text_normalized` usable as a plain column in group/order/aggregate contexts.
--
-- Width and kana variants are handled by NFKC-normalizing at ingestion, not
-- here: neither tokenizer folds halfwidth kana, so an unnormalized query would
-- simply miss.
CREATE INDEX extracted_contents_bm25 ON extracted_contents
USING bm25 (
    content_version_id,
    (text_normalized::pdb.lindera(japanese)),
    (text_normalized::pdb.ngram(2, 3, 'alias=text_ngram'))
) WITH (key_field = 'content_version_id');
