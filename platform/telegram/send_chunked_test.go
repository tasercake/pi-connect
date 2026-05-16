package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitForTelegram_ShortContent_NotSplit(t *testing.T) {
	in := "hello world"
	got := splitForTelegram(in, 100)
	if len(got) != 1 || got[0] != in {
		t.Fatalf("expected single chunk %q, got %#v", in, got)
	}
}

func TestSplitForTelegram_SplitsAtParagraphBoundary(t *testing.T) {
	a := strings.Repeat("alpha ", 100)
	b := strings.Repeat("bravo ", 100)
	in := strings.TrimSpace(a) + "\n\n" + strings.TrimSpace(b)
	got := splitForTelegram(in, 700)
	if len(got) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(got))
	}
	if !strings.HasSuffix(got[0], "alpha") {
		t.Fatalf("first chunk did not end on paragraph boundary: %q", tail(got[0], 40))
	}
	if !strings.HasPrefix(got[1], "bravo") {
		t.Fatalf("second chunk did not start at next paragraph: %q", head(got[1], 40))
	}
}

func TestSplitForTelegram_FallsBackToLine(t *testing.T) {
	// No paragraph break; just line breaks.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("line-of-text-here\n")
	}
	got := splitForTelegram(sb.String(), 500)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	// Every chunk should end at a line boundary (no trailing partial words).
	for i, c := range got {
		if strings.Contains(c, "line-of-text-her\n") || strings.HasSuffix(c, "line-of-text-her") {
			t.Errorf("chunk %d split mid-line: tail=%q", i, tail(c, 30))
		}
	}
}

func TestSplitForTelegram_FallsBackToSentence(t *testing.T) {
	// Single paragraph, no newlines, sentence-end punctuation present.
	sentence := strings.Repeat("word ", 30) + "."
	in := strings.Repeat(sentence+" ", 50)
	got := splitForTelegram(in, 400)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	// First chunk should end with "." (possibly followed by trimmed space).
	if !strings.HasSuffix(strings.TrimRight(got[0], " "), ".") {
		t.Errorf("first chunk did not end at sentence boundary: tail=%q", tail(got[0], 40))
	}
}

func TestSplitForTelegram_AllChunksUnderBudget(t *testing.T) {
	in := strings.Repeat("x ", 5000) // 10k chars, no useful boundaries
	got := splitForTelegram(in, 600)
	if len(got) == 0 {
		t.Fatal("got zero chunks")
	}
	for i, c := range got {
		if r := utf8.RuneCountInString(c); r > 600 {
			t.Errorf("chunk %d exceeded budget: %d runes", i, r)
		}
	}
	joined := strings.ReplaceAll(strings.Join(got, " "), "  ", " ")
	if strings.Count(joined, "x") != strings.Count(in, "x") {
		t.Errorf("lost x tokens after split: got=%d want=%d", strings.Count(joined, "x"), strings.Count(in, "x"))
	}
}

func TestSplitForTelegram_HandlesUnicode(t *testing.T) {
	// Each rune is multi-byte; budget must be enforced in runes, not bytes.
	in := strings.Repeat("\u4e2d\u6587 ", 1000) // CJK chars
	got := splitForTelegram(in, 200)
	for i, c := range got {
		if r := utf8.RuneCountInString(c); r > 200 {
			t.Errorf("chunk %d exceeded rune budget: %d", i, r)
		}
	}
}

func TestFindBestSplit_PrefersParagraph(t *testing.T) {
	in := strings.Repeat("a", 100) + "\n\n" + strings.Repeat("b", 100)
	cut := findBestSplit(in, 150)
	if in[cut-2:cut] != "\n\n" {
		t.Errorf("expected cut just after paragraph break, got context=%q", in[cut-5:cut+5])
	}
}

func TestFindBestSplit_FallsThroughToHardCut(t *testing.T) {
	in := strings.Repeat("a", 1000) // no boundaries at all
	cut := findBestSplit(in, 300)
	if cut != 300 {
		t.Errorf("expected hard cut at byte 300, got %d", cut)
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
