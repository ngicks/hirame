package doctype_test

import (
	"slices"
	"testing"

	"github.com/ngicks/hirame/apps/search-api/internal/doctype"
)

func TestAllowPathAcceptsTheDefaultDocumentSet(t *testing.T) {
	filter := doctype.NewFilter(nil)
	for _, path := range []string{
		"/srv/docs/report.pdf",
		"/srv/docs/memo.docx",
		"/srv/docs/sheet.XLSX",
		"/srv/docs/notes.md",
		"/srv/docs/mail.eml",
		"/srv/docs/deep/nested/paper.odt",
	} {
		if !filter.AllowPath(path) {
			t.Errorf("AllowPath(%q) = false, want true", path)
		}
	}
}

func TestAllowPathRejectsWhatNeitherTikaNorGahakuWouldUse(t *testing.T) {
	filter := doctype.NewFilter(nil)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"disk image", "/srv/docs/backup.img"},
		{"archive", "/srv/docs/bundle.tar.gz"},
		{"image", "/srv/docs/scan.png"},
		{"no extension", "/srv/docs/README"},
		{"vim swap file", "/srv/docs/.report.pdf.swp"},
		{"emacs autosave", "/srv/docs/.#report.pdf"},
		{"word lock file", "/srv/docs/~$report.docx"},
		{"editor backup", "/srv/docs/report.pdf~"},
		{"partial download", "/srv/docs/report.pdf.part"},
		{"libreoffice lock", "/srv/docs/.~lock.report.pdf#"},
		{"hidden document", "/srv/docs/.secret.pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if filter.AllowPath(tc.path) {
				t.Errorf("AllowPath(%q) = true, want false", tc.path)
			}
		})
	}
}

func TestNewFilterNormalizesTheConfiguredSet(t *testing.T) {
	filter := doctype.NewFilter([]string{".PDF", "txt", "  .Md  ", ""})

	if got := filter.Extensions(); !slices.Equal(got, []string{"md", "pdf", "txt"}) {
		t.Fatalf("Extensions() = %v", got)
	}
	if !filter.AllowPath("/srv/docs/a.pdf") {
		t.Error("a configured extension written with a dot and in caps was rejected")
	}
	if filter.AllowPath("/srv/docs/a.docx") {
		t.Error("an extension outside the configured set was accepted")
	}
}

func TestContentTypeAnnouncesTheExtensionsTypeRatherThanTheSniff(t *testing.T) {
	filter := doctype.NewFilter(nil)

	// A .docx is a ZIP on the wire; sniffing would report application/zip and
	// tell Tika nothing useful.
	got, ok := filter.ContentType("/srv/docs/memo.docx", []byte("PK\x03\x04rest"))
	if !ok {
		t.Fatal("ContentType rejected a .docx")
	}
	want := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	if got != want {
		t.Errorf("ContentType = %q, want %q", got, want)
	}
}

func TestContentTypeRejectsBinaryBytesUnderATextExtension(t *testing.T) {
	filter := doctype.NewFilter(nil)

	if _, ok := filter.ContentType("/srv/docs/notes.txt", []byte{0, 1, 2, 3, 4}); ok {
		t.Error("binary content under a .txt was accepted")
	}
	if _, ok := filter.ContentType("/srv/docs/notes.txt", []byte("plain words")); !ok {
		t.Error("real text under a .txt was rejected")
	}
}

// An office document is a ZIP container, so the sniff must never be allowed to
// reject one — that would drop every .docx, .xlsx, and .pptx in the tree.
func TestContentTypeNeverRejectsAZipBackedOfficeDocument(t *testing.T) {
	filter := doctype.NewFilter(nil)
	for _, path := range []string{"/a/x.docx", "/a/x.xlsx", "/a/x.pptx", "/a/x.odt"} {
		if _, ok := filter.ContentType(path, []byte("PK\x03\x04\x00\x00binary\x00")); !ok {
			t.Errorf("ContentType(%q) rejected a ZIP-backed document", path)
		}
	}
}

func TestContentTypeAllowsAnEmptyFile(t *testing.T) {
	filter := doctype.NewFilter(nil)
	if _, ok := filter.ContentType("/srv/docs/a.pdf", nil); !ok {
		t.Error("an empty document was rejected; truncation is a state to record")
	}
}
