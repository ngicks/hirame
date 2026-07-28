// Package doctype decides which watched files become documents and what media
// type they are announced as.
//
// The gate exists because a watched mountpoint is a whole filesystem: without
// it every lock file, swap file, log, and disk image would be hashed, uploaded
// to Tika, and stored. The default set is therefore what Tika extracts text
// from and Gahaku renders (PDF directly, everything else through soffice), and
// nothing else.
package doctype

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
)

// SniffLen is how many leading bytes [Filter.ContentType] wants. It matches
// what http.DetectContentType actually consumes.
const SniffLen = 512

// DefaultExtensions is the conservative include set, lowercase and dot-less.
//
// Images are deliberately absent: Tika returns no text for them without an OCR
// parser configured, and Gahaku renders PDF and office documents only, so an
// included image would cost a hash and an upload to produce an empty document.
// Archives, media, and source code are absent for the same reason.
func DefaultExtensions() []string {
	return []string{
		"pdf",
		"doc", "docx", "xls", "xlsx", "ppt", "pptx",
		"odt", "ods", "odp",
		"rtf", "epub",
		"txt", "md", "markdown", "csv", "tsv",
		"json", "xml", "html", "htm", "xhtml",
		"eml", "msg",
	}
}

// extensionTypes maps the include set to the media type handed to Tika.
// Tika detects the type itself when none is sent, but detection from a stream
// is weaker than the name the filesystem already gave us.
var extensionTypes = map[string]string{
	"pdf":  "application/pdf",
	"doc":  "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":  "application/vnd.ms-excel",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"ppt":  "application/vnd.ms-powerpoint",
	"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"odt":  "application/vnd.oasis.opendocument.text",
	"ods":  "application/vnd.oasis.opendocument.spreadsheet",
	"odp":  "application/vnd.oasis.opendocument.presentation",
	"rtf":  "application/rtf",
	"epub": "application/epub+zip",

	"txt":      "text/plain",
	"md":       "text/markdown",
	"markdown": "text/markdown",
	"csv":      "text/csv",
	"tsv":      "text/tab-separated-values",
	"json":     "application/json",
	"xml":      "application/xml",
	"html":     "text/html",
	"htm":      "text/html",
	"xhtml":    "application/xhtml+xml",

	"eml": "message/rfc822",
	"msg": "application/vnd.ms-outlook",
}

// textLike is the subset whose bytes must actually be text. It is the only
// place a content sniff may reject: the office formats are ZIP containers, so
// sniffing cannot tell a .docx from any other archive and would reject every
// one of them.
var textLike = map[string]bool{
	"txt": true, "md": true, "markdown": true, "csv": true, "tsv": true,
	"json": true, "xml": true, "html": true, "htm": true, "xhtml": true,
}

// Filter is an immutable include set. The zero value allows nothing; build one
// with [NewFilter].
type Filter struct {
	allowed map[string]struct{}
}

// NewFilter builds a Filter over the given extensions, which may be written
// with or without a leading dot and in any case. An empty slice selects
// [DefaultExtensions].
func NewFilter(extensions []string) *Filter {
	if len(extensions) == 0 {
		extensions = DefaultExtensions()
	}
	allowed := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		norm := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
		if norm != "" {
			allowed[norm] = struct{}{}
		}
	}
	return &Filter{allowed: allowed}
}

// Extensions returns the include set, sorted, for logging.
func (f *Filter) Extensions() []string {
	out := make([]string, 0, len(f.allowed))
	for ext := range f.allowed {
		out = append(out, ext)
	}
	slices.Sort(out)
	return out
}

// AllowPath reports whether a path is worth opening at all. It is the cheap
// gate the event loop applies, before anything reads the file.
func (f *Filter) AllowPath(path string) bool {
	base := filepath.Base(path)
	// Editors and office suites park their scratch state beside the document
	// under a name that keeps the real extension: emacs writes `.#report.pdf`
	// and Word writes `~$report.docx`, both of which the extension test alone
	// would wave through. Backup and partial-download names (`report.pdf~`,
	// `report.pdf.part`) need no special case, because their extension is not
	// the document's.
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "~$") {
		return false
	}
	_, ok := f.allowed[extensionOf(base)]
	return ok
}

// ContentType returns the media type to announce to Tika, and whether the file
// should still be ingested now that its leading bytes are known.
//
// head may be short or empty; an empty file is allowed through, because a
// document truncated to zero is a real state the extraction result should
// record rather than something to silently drop.
func (f *Filter) ContentType(path string, head []byte) (string, bool) {
	if !f.AllowPath(path) {
		return "", false
	}
	ext := extensionOf(filepath.Base(path))
	if textLike[ext] && len(head) > 0 && isBinary(head) {
		return "", false
	}
	if declared, ok := extensionTypes[ext]; ok {
		return declared, true
	}
	if len(head) == 0 {
		return "", true
	}
	return http.DetectContentType(head), true
}

// isBinary reports whether head sniffs as something other than text. Go's
// sniffer answers "application/octet-stream" exactly when the bytes contain
// control characters no text encoding uses.
func isBinary(head []byte) bool {
	return http.DetectContentType(head) == "application/octet-stream"
}

func extensionOf(base string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(base), "."))
}
