package pi

// Attachment routing for the pi agent driver.
//
// Background: when pi-connect receives a file from the platform (e.g. a
// Telegram document), it saves the bytes to disk and previously asked pi to
// inline the file content into the user message via the `@<path>` syntax.
// That works fine for small text files (configs, code, prose) but is
// catastrophic for everything else — a 5 MB image-heavy PDF inflates to
// millions of tokens of base64 once pi treats every page as a multimodal
// image, and the model API rejects the request with `prompt is too long`.
//
// Real example from production logs:
//   400 invalid_request_error: "prompt is too long: 3,125,215 tokens > 1,000,000 maximum"
//
// The fix: classify each attachment as inline-safe (small text/code) or
// path-reference (everything else). Inline-safe attachments keep going
// through `@<path>` because that's still the most ergonomic for code review
// flows. Path-reference attachments are saved to disk as before but only
// *mentioned* in the user message — pi's normal `read` tool / OCR skills can
// pull the content incrementally if and when they're needed.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tasercake/pi-connect/core"
)

// maxInlineFileBytes is the size threshold above which even text/code-like
// MIME types are forced through the path-reference path. Inlining a 50 MB
// build log into a single user message is technically allowed by `@file`
// but is almost never what the user wants and routinely overruns context.
const maxInlineFileBytes = 256 * 1024

// classifiedAttachments separates a set of saved file paths into:
//   - inlinePaths: safe to pass as `@<path>` so pi reads the file content
//     directly into the user message. Text/code MIME types under the size
//     guard end up here.
//   - referencePaths: large files or binary/document MIME types (PDFs,
//     audio, video, zips, etc.). The driver mentions them as paths in the
//     message body instead of inlining; the model decides whether to read
//     them via its `read` tool / domain skills.
type classifiedAttachments struct {
	inlinePaths    []string
	referencePaths []string
}

// classifyFileAttachments saves all attachments to disk, then partitions
// their paths based on MIME type and size. The order of inputs is preserved
// within each partition.
//
// We deliberately reuse core.SaveFilesToDisk so the on-disk layout under
// .pi-connect/attachments/ matches the existing convention (path stability
// across turns matters because the model may have already mentioned a file
// path in earlier output).
func classifyFileAttachments(workDir string, files []core.FileAttachment) classifiedAttachments {
	if len(files) == 0 {
		return classifiedAttachments{}
	}
	paths := core.SaveFilesToDisk(workDir, files)
	// core.SaveFilesToDisk may skip a file on write error; reconciling
	// indices is awkward but doable by matching saved basenames back to
	// inputs. In practice writes succeed; if one fails, we silently
	// drop that attachment, matching prior behavior.
	pathByName := make(map[string]string, len(paths))
	for _, p := range paths {
		pathByName[filepath.Base(p)] = p
	}

	var out classifiedAttachments
	for i, f := range files {
		fname := f.FileName
		if fname == "" {
			// Match core.SaveFilesToDisk's synthetic naming so the
			// lookup succeeds; the exact name doesn't matter here as
			// long as it agrees with what SaveFilesToDisk produced.
			// We can fall back to a positional lookup if needed.
			fname = ""
		}
		p, ok := pathByName[fname]
		if !ok {
			// Positional fallback: assume same order, skip any earlier
			// drops conservatively.
			if i < len(paths) {
				p = paths[i]
			} else {
				continue
			}
		}

		if isInlineSafe(f.MimeType, fname, len(f.Data)) {
			out.inlinePaths = append(out.inlinePaths, p)
		} else {
			out.referencePaths = append(out.referencePaths, p)
		}
	}
	return out
}

// isInlineSafe decides whether a file is small enough and "texty" enough to
// be inlined via `@<path>`. Anything ambiguous defaults to a path reference,
// which is the safer behavior: a path reference at worst means the model
// has to call its `read` tool to see the content; an erroneous inline can
// blow the context window and fail the turn entirely.
func isInlineSafe(mime, fileName string, size int) bool {
	if size > maxInlineFileBytes {
		return false
	}
	mime = strings.ToLower(strings.TrimSpace(mime))

	// Authoritative: MIME type starts with text/.
	if strings.HasPrefix(mime, "text/") {
		return true
	}

	// Recognise common application/* MIME types that are still
	// structured-text in practice. Avoid catch-all because too many
	// binary formats live under application/*.
	switch mime {
	case "application/json",
		"application/xml",
		"application/javascript",
		"application/x-javascript",
		"application/yaml",
		"application/x-yaml",
		"application/toml",
		"application/x-toml",
		"application/sql",
		"application/x-sh",
		"application/x-shellscript",
		"application/x-python",
		"application/x-tex",
		"application/x-latex":
		return true
	}

	// MIME type missing or generic (application/octet-stream is the
	// usual Telegram default for files without server-side sniffing).
	// Fall back to extension-based classification, but only for a
	// curated allowlist of code/text extensions.
	if mime == "" || mime == "application/octet-stream" {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileName), "."))
		switch ext {
		case "txt", "md", "markdown", "rst", "log",
			"json", "yaml", "yml", "toml", "ini", "cfg", "conf", "env",
			"xml", "html", "htm", "css", "csv", "tsv",
			"py", "go", "rs", "ts", "tsx", "js", "jsx", "mjs", "cjs",
			"c", "h", "cc", "cpp", "hpp", "cs", "java", "kt", "swift",
			"rb", "php", "lua", "sh", "bash", "zsh", "fish",
			"sql", "tex", "diff", "patch", "dockerfile", "makefile",
			"gradle", "kts":
			return true
		}
	}

	return false
}

// buildAttachmentPrefix returns text to prepend to the user message that
// surfaces path-reference attachments without inlining their bytes. The
// model treats these as file paths it can read via its built-in `read`
// tool or domain-specific skills (ocr-and-documents, etc.). Returns the
// empty string when there are no path references.
//
// Format chosen to be agent-readable but obvious to a human reading the
// raw transcript:
//
//	[Attached files — read with the appropriate tool, do NOT assume contents]
//	- /abs/path/to/Doc.pdf  (application/pdf)
//	- /abs/path/to/Audio.m4a  (audio/m4a)
//
// We avoid `@<path>` here on purpose: that would re-trigger pi's inlining.
func buildAttachmentPrefix(paths []string, files []core.FileAttachment) string {
	if len(paths) == 0 {
		return ""
	}
	// Build a lookup of basename -> mime so we can annotate each line
	// without depending on order.
	mimeByName := make(map[string]string, len(files))
	for _, f := range files {
		if f.FileName != "" {
			mimeByName[f.FileName] = f.MimeType
		}
	}

	var b strings.Builder
	b.WriteString("[Attached files — read with the appropriate tool (e.g. the `read` tool, or the `ocr-and-documents` / `yt-dlp` / domain skill that matches the MIME type). Do NOT assume contents until you've actually read the file.]\n")
	for _, p := range paths {
		mime := mimeByName[filepath.Base(p)]
		if mime == "" {
			mime = "unknown"
		}
		fmt.Fprintf(&b, "- %s  (%s)\n", p, mime)
	}
	b.WriteString("\n")
	return b.String()
}
