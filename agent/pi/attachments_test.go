package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tasercake/pi-connect/core"
)

func TestIsInlineSafe_TextMime(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		{"text/plain", true},
		{"text/markdown", true},
		{"text/x-python", true},
		{"text/csv", true},
		{"application/json", true},
		{"application/yaml", true},
		{"application/x-yaml", true},
		{"application/toml", true},
		{"application/javascript", true},
		{"application/x-tex", true},
		{"application/pdf", false},
		{"application/zip", false},
		{"application/msword", false},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", false},
		{"audio/mpeg", false},
		{"audio/m4a", false},
		{"video/mp4", false},
		{"image/png", false}, // images go through saveImagesToDisk
		{"", false},          // no extension info → defaults to reference
	}
	for _, c := range cases {
		if got := isInlineSafe(c.mime, "", 100); got != c.want {
			t.Errorf("isInlineSafe(%q) = %v, want %v", c.mime, got, c.want)
		}
	}
}

func TestIsInlineSafe_SizeGuard(t *testing.T) {
	// Even text/plain gets pushed to reference path if over budget.
	if isInlineSafe("text/plain", "huge.log", maxInlineFileBytes+1) {
		t.Error("size guard not enforced for text/plain over budget")
	}
	if !isInlineSafe("text/plain", "small.txt", maxInlineFileBytes-1) {
		t.Error("under-budget text/plain should be inline-safe")
	}
}

func TestIsInlineSafe_OctetStreamFallsBackToExtension(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"config.yaml", true},
		{"main.go", true},
		{"script.sh", true},
		{"Makefile", false}, // no extension → conservative reject
		{"doc.pdf", false},
		{"clip.mp4", false},
		{"deck.pptx", false},
		{"unknown.xyz", false},
		{"file.PY", true}, // case-insensitive
	}
	for _, c := range cases {
		got := isInlineSafe("application/octet-stream", c.name, 1024)
		if got != c.want {
			t.Errorf("isInlineSafe(octet-stream, %q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClassifyFileAttachments_SplitsByMime(t *testing.T) {
	tmp := t.TempDir()
	files := []core.FileAttachment{
		{FileName: "notes.md", MimeType: "text/markdown", Data: []byte("# hi")},
		{FileName: "deck.pdf", MimeType: "application/pdf", Data: []byte("%PDF-1.4 not really")},
		{FileName: "audio.m4a", MimeType: "audio/m4a", Data: []byte("\x00\x00")},
		{FileName: "schema.json", MimeType: "application/json", Data: []byte(`{"a":1}`)},
	}
	got := classifyFileAttachments(tmp, files)

	if len(got.inlinePaths) != 2 {
		t.Fatalf("expected 2 inline, got %d: %v", len(got.inlinePaths), got.inlinePaths)
	}
	if len(got.referencePaths) != 2 {
		t.Fatalf("expected 2 reference, got %d: %v", len(got.referencePaths), got.referencePaths)
	}
	// Verify on-disk paths actually exist.
	for _, p := range append(got.inlinePaths, got.referencePaths...) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file at %s, stat err: %v", p, err)
		}
	}
	// Verify expected file went to expected bucket.
	gotInline := joinedBases(got.inlinePaths)
	gotRef := joinedBases(got.referencePaths)
	if !strings.Contains(gotInline, "notes.md") || !strings.Contains(gotInline, "schema.json") {
		t.Errorf("text files missing from inline set: %v", gotInline)
	}
	if !strings.Contains(gotRef, "deck.pdf") || !strings.Contains(gotRef, "audio.m4a") {
		t.Errorf("binary files missing from reference set: %v", gotRef)
	}
}

func TestClassifyFileAttachments_EmptyInput(t *testing.T) {
	got := classifyFileAttachments(t.TempDir(), nil)
	if len(got.inlinePaths)+len(got.referencePaths) != 0 {
		t.Errorf("expected empty result for nil input, got %+v", got)
	}
}

func TestBuildAttachmentPrefix_EmptyReturnsBlank(t *testing.T) {
	if buildAttachmentPrefix(nil, nil) != "" {
		t.Error("expected empty prefix for no paths")
	}
}

func TestBuildAttachmentPrefix_FormatsPathsWithMime(t *testing.T) {
	files := []core.FileAttachment{
		{FileName: "deck.pdf", MimeType: "application/pdf"},
		{FileName: "audio.m4a", MimeType: "audio/m4a"},
	}
	paths := []string{"/tmp/x/deck.pdf", "/tmp/x/audio.m4a"}
	got := buildAttachmentPrefix(paths, files)
	if !strings.Contains(got, "Attached files") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "/tmp/x/deck.pdf  (application/pdf)") {
		t.Errorf("PDF line missing or malformed: %q", got)
	}
	if !strings.Contains(got, "/tmp/x/audio.m4a  (audio/m4a)") {
		t.Errorf("audio line missing or malformed: %q", got)
	}
	if !strings.Contains(got, "Do NOT assume contents") {
		t.Error("missing instruction to read before assuming")
	}
}

func joinedBases(paths []string) string {
	var bs []string
	for _, p := range paths {
		bs = append(bs, filepath.Base(p))
	}
	return strings.Join(bs, " ")
}
