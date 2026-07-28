-- BM25 search over the pg_search index defined in migration 0002 (D-012).
--
-- Both tokenizations of text_normalized are ORed: the Lindera field supplies
-- the relevance signal, the ngram field supplies recall for short queries and
-- terms the dictionary does not segment. `|||` is pg_search's match-any
-- operator, so a multi-term query behaves as OR within each field too.
--
-- The Lindera side is boosted (D-012's validation result). Left unweighted, an
-- ngram false positive outranks the correct segmentation: `アパート` reaches a
-- document holding `レポート` through the shared `ート` bigram, and scored
-- higher than the document that actually contains the word. The boost keeps
-- the ngram field's recall while demoting it below any dictionary match.
--
-- 16 is measured, not chosen: against pg_search 0.24.3 the inversion above
-- survives a factor of 12 and is corrected at 16. It is deliberately large
-- because the two fields are not two opinions about relevance — the ngram field
-- is a recall net, and its score is only meant to order documents Lindera did
-- not reach at all. Scaling one side cannot reorder two documents that both
-- match through Lindera, so a generous factor costs nothing there.
--
-- The boosted side is spelled `@@@ pdb.match_disjunction($1)::pdb.boost(16)`
-- rather than the `||| $1::text::pdb.boost(16)` the documentation shows,
-- because the documented form cannot survive a prepared statement. Casting a
-- bind parameter to pdb.boost plans only while PostgreSQL still uses a custom
-- plan; on the sixth execution it switches to a generic plan and the operator
-- fails outright with "the right-hand side of the `|||` operator must be a text
-- or text array value". Every driver here prepares, so that is not an edge
-- case — it is search breaking after five queries. Handing the parameter to the
-- query-builder function instead keeps it a plain text argument, and
-- match_disjunction is what `|||` already means. Verified identical in score
-- and stable over twelve executions of one prepared statement.
--
-- The caller must NFKC-normalize the query the same way ingestion normalizes
-- the text, or halfwidth/fullwidth variants will not meet.
--
-- Ordering is (score DESC, document id ASC): the id tiebreaker is what makes
-- LIMIT/OFFSET paging repeatable when several documents score identically,
-- which duplicate content makes common here.
--
-- pg_search 0.24.3 only highlights the un-aliased field, so a row reached only
-- through the ngram field has no snippet at all — pdb.snippet returns NULL for
-- it, and asking for the snippet of the aliased field does not help. The
-- COALESCE falls back to a leading excerpt rather than exposing a nullable
-- column, because "recall hit, nothing to highlight" is a normal result here,
-- not a missing value the caller should have to branch on.

-- name: SearchDocuments :many
SELECT
    d.id                                    AS document_id,
    d.mountpoint_id,
    m.root_path,
    d.path,
    ec.content_version_id,
    cv.content_hash,
    cv.size_bytes,
    ec.content_type,
    pdb.score(ec.content_version_id)::real  AS score,
    COALESCE(
        pdb.snippet(ec.text_normalized),
        left(ec.text_normalized, 200)
    )::text                                 AS snippet
FROM extracted_contents ec
JOIN documents d ON d.current_content_version_id = ec.content_version_id
JOIN content_versions cv ON cv.id = ec.content_version_id
JOIN mountpoints m ON m.id = d.mountpoint_id
WHERE (
        ec.text_normalized @@@ pdb.match_disjunction(@query::text)::pdb.boost(16)
        OR ec.text_normalized::pdb.alias('text_ngram') ||| @query::text
      )
  AND ec.status = 'succeeded'
  AND d.deleted_at IS NULL
ORDER BY score DESC, document_id ASC
LIMIT @result_limit OFFSET @result_offset;

-- CountSearchDocuments must select exactly the rows SearchDocuments does.
--
-- Its WHERE is spelled differently on purpose, and the difference is not a
-- drift to be repaired: a boost weights a match rather than admitting one, so
-- the boosted and unboosted forms match the same rows, and `|||` here is the
-- same disjunction `pdb.match_disjunction` builds there. Only the boosted form
-- needs the query-builder spelling, because only a cast to pdb.boost breaks
-- under a generic plan. Counting without the boost keeps this query off that
-- hazard entirely and costs nothing, since the count does not score.
-- name: CountSearchDocuments :one
SELECT count(*)
FROM extracted_contents ec
JOIN documents d ON d.current_content_version_id = ec.content_version_id
WHERE (
        ec.text_normalized ||| @query::text
        OR ec.text_normalized::pdb.alias('text_ngram') ||| @query::text
      )
  AND ec.status = 'succeeded'
  AND d.deleted_at IS NULL;

-- SearchDocumentsLindera isolates the dictionary-segmented field. It exists so
-- the D-012 quality follow-up can compare the two fields' behaviour on real
-- documents rather than only their union.
-- name: SearchDocumentsLindera :many
SELECT
    d.id                                    AS document_id,
    d.path,
    pdb.score(ec.content_version_id)::real  AS score,
    COALESCE(
        pdb.snippet(ec.text_normalized),
        left(ec.text_normalized, 200)
    )::text                                 AS snippet
FROM extracted_contents ec
JOIN documents d ON d.current_content_version_id = ec.content_version_id
WHERE ec.text_normalized ||| @query::text
  AND ec.status = 'succeeded'
  AND d.deleted_at IS NULL
ORDER BY score DESC, document_id ASC
LIMIT @result_limit OFFSET @result_offset;

-- name: SearchDocumentsNgram :many
SELECT
    d.id                                    AS document_id,
    d.path,
    pdb.score(ec.content_version_id)::real  AS score,
    COALESCE(
        pdb.snippet(ec.text_normalized),
        left(ec.text_normalized, 200)
    )::text                                 AS snippet
FROM extracted_contents ec
JOIN documents d ON d.current_content_version_id = ec.content_version_id
WHERE ec.text_normalized::pdb.alias('text_ngram') ||| @query::text
  AND ec.status = 'succeeded'
  AND d.deleted_at IS NULL
ORDER BY score DESC, document_id ASC
LIMIT @result_limit OFFSET @result_offset;
