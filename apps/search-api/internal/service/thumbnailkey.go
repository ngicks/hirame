package service

import "fmt"

// The cache key is a stored contract, not an implementation detail: object_key
// is written into every thumbnail_cache row, and the accounting row is the only
// way a stored object can be reached. Changing the scheme below strands every
// object already in the bucket behind a key nothing derives any more, so it
// lives apart from the handler that happens to build it.

// entry is one resolved cache key: the ref plus the spec, which is exactly
// what addresses a stored object.
type entry struct {
	contentVersionID int64
	contentHash      string
	page             uint32
	bounds           imageBounds
	format           imageFormat
}

// objectKey is derived from the content hash rather than the document, so a
// rename cannot orphan an object and identical bytes under two paths share one
// entry. It is stored in the accounting row, which stays the only way to reach
// it.
func (e entry) objectKey() string {
	return fmt.Sprintf(
		"thumbnails/%s/p%d/%dx%d.%s",
		e.contentHash, e.page, e.bounds.width, e.bounds.height, e.format.name,
	)
}

func (e entry) singleflightKey() string { return e.objectKey() }
