package store

import "golang.org/x/text/unicode/norm"

// NormalizeText applies the NFKC normalization the BM25 index assumes (D-012).
//
// It lives beside the schema rather than in the indexer because both sides of
// the contract need it: text written to extracted_contents.text_normalized and
// the query string handed to SearchDocuments must be folded the same way, or
// halfwidth kana, fullwidth latin, and kana variants simply never meet. Neither
// the Lindera nor the ngram tokenizer folds them.
func NormalizeText(s string) string {
	return norm.NFKC.String(s)
}
