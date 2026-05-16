package telegram

// Long-message handling for Telegram.
//
// Telegram rejects messages over 4096 UTF-16 code units per `sendMessage` call
// with `Bad Request: message is too long`. Until this file existed, cc-connect
// just propagated that error up and the user never saw the reply.
//
// We do two things here:
//   1. splitForTelegram: split source content at natural boundaries
//      (paragraph -> line -> sentence -> word -> hard cut) so each chunk is
//      safely under the limit.
//   2. sendChunked: convert each chunk to HTML, send sequentially with the
//      same per-chunk "HTML rejected" -> plain-text fallback the original
//      code already had. Optionally prefix multi-chunk replies with "(i/N) "
//      so the user knows there's more coming.
//
// Why a conservative budget (3500 chars, not 4096):
//   - Telegram's limit is in UTF-16 code units. Most BMP characters are 1
//     unit but surrogate-pair characters (some emoji, supplementary scripts)
//     count as 2.
//   - HTML conversion can expand certain chars ("<" -> "&lt;", etc.) which
//     adds to the wire length.
//   - We've seen Telegram reject a 2782-char message — likely an entity
//     overflow disguised as "too long". A wider safety margin is cheap.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// telegramChunkSourceBudget is the rune budget per chunk in the source
// (pre-HTML) string. Kept well below Telegram's 4096-UTF-16-unit limit to
// leave room for HTML entity expansion and surrogate pairs.
const telegramChunkSourceBudget = 3500

// splitForTelegram divides content into chunks each at most maxLen runes long,
// preferring natural boundaries in this priority order:
//
//	paragraph break ("\n\n") -> line break ("\n") -> sentence end (.!?) ->
//	word boundary (" ") -> hard rune cut.
//
// Each chunk's leading/trailing whitespace is trimmed except where doing so
// would empty it.
func splitForTelegram(content string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = telegramChunkSourceBudget
	}
	if utf8.RuneCountInString(content) <= maxLen {
		return []string{content}
	}

	var chunks []string
	remaining := content
	for utf8.RuneCountInString(remaining) > maxLen {
		cut := findBestSplit(remaining, maxLen)
		if cut <= 0 {
			// Defensive: never make zero progress.
			cut = byteIndexAtRune(remaining, maxLen)
		}
		head := strings.TrimRight(remaining[:cut], " \t\r\n")
		if head != "" {
			chunks = append(chunks, head)
		}
		remaining = strings.TrimLeft(remaining[cut:], " \t\r\n")
	}
	if strings.TrimSpace(remaining) != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

// findBestSplit returns a byte index in s where the chunk should end. The
// returned index is no later than the byte position of the maxLen-th rune.
// It prefers, in order: the last "\n\n" beyond the halfway mark, then "\n",
// then sentence-ending punctuation, then a space, and finally the hard cut.
func findBestSplit(s string, maxLen int) int {
	hardByte := byteIndexAtRune(s, maxLen)
	if hardByte == len(s) {
		return hardByte
	}
	head := s[:hardByte]
	halfByte := byteIndexAtRune(s, maxLen/2)

	if i := strings.LastIndex(head, "\n\n"); i >= halfByte {
		return i + 2
	}
	if i := strings.LastIndexByte(head, '\n'); i >= halfByte {
		return i + 1
	}
	if i := lastIndexAnyByte(head, ".!?"); i >= halfByte {
		// Include the punctuation, plus a following space if present.
		end := i + 1
		if end < len(s) && s[end] == ' ' {
			end++
		}
		return end
	}
	if i := strings.LastIndexByte(head, ' '); i >= halfByte {
		return i + 1
	}
	return hardByte
}

// byteIndexAtRune returns the byte offset of the n-th rune (0-indexed), or
// len(s) if s has fewer than n+1 runes.
func byteIndexAtRune(s string, n int) int {
	if n <= 0 {
		return 0
	}
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}

// lastIndexAnyByte is a byte-only variant of strings.LastIndexAny restricted
// to ASCII chars in `chars`. Good enough for our punctuation set.
func lastIndexAnyByte(s, chars string) int {
	for i := len(s) - 1; i >= 0; i-- {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return i
			}
		}
	}
	return -1
}

// chunkSendOptions captures the per-call extras that can't be shared across
// all chunks. Reply parameters apply to the first chunk only (subsequent
// chunks would create N visual replies, which is noisy). Reply markup
// (inline keyboards) applies to the last chunk only — the buttons belong
// next to the action they're about.
type chunkSendOptions struct {
	replyTo      *models.ReplyParameters
	replyMarkup  *models.InlineKeyboardMarkup
	chatID       int64
	threadID     int
	logMethod    string // for slog tagging on fallback paths
	logChunkInfo bool   // adds (i/N) prefix to chunked sends
}

// sendChunked converts content to HTML, splits the source if it exceeds the
// Telegram limit, and sends each chunk sequentially. Within a chunk:
//   - First try HTML; on "can't parse", retry as plain text (legacy behavior).
//   - If a single chunk is still rejected as too long (e.g. UTF-16 unit
//     accounting differs from our rune-based estimate), split that chunk in
//     half and retry up to a small depth.
//
// Returns the first error encountered. Subsequent chunks are not sent after
// an error, mirroring the original "fail closed" behavior — but at least the
// user will have seen everything that did get through.
func (p *Platform) sendChunked(ctx context.Context, bot telegramBot, content string, opts chunkSendOptions) error {
	chunks := splitForTelegram(content, telegramChunkSourceBudget)
	n := len(chunks)

	for i, chunk := range chunks {
		body := chunk
		if opts.logChunkInfo && n > 1 {
			body = fmt.Sprintf("(%d/%d) %s", i+1, n, chunk)
		}

		params := &tgbot.SendMessageParams{
			ChatID:          opts.chatID,
			MessageThreadID: opts.threadID,
			Text:            core.MarkdownToSimpleHTML(body),
			ParseMode:       models.ParseModeHTML,
		}
		if i == 0 && opts.replyTo != nil {
			params.ReplyParameters = opts.replyTo
		}
		if i == n-1 && opts.replyMarkup != nil {
			params.ReplyMarkup = opts.replyMarkup
		}

		if err := sendOneChunkWithFallback(ctx, bot, params, body, opts.logMethod); err != nil {
			return err
		}
	}
	return nil
}

// sendOneChunkWithFallback sends a single message. It applies:
//  1. The original HTML -> plain-text fallback on "can't parse" errors.
//  2. A defensive "too long" fallback that splits the chunk in half and
//     recurses up to one extra split, in case our pre-split budget was off.
func sendOneChunkWithFallback(ctx context.Context, bot telegramBot, params *tgbot.SendMessageParams, plainBody, logMethod string) error {
	_, err := bot.SendMessage(ctx, params)
	if err == nil {
		return nil
	}
	errStr := err.Error()

	if strings.Contains(errStr, "can't parse") {
		slog.Warn("telegram: HTML rejected, retrying as plain text",
			"method", logMethod, "error", errStr,
			"html_prefix", truncateForLog(params.Text, 200),
			"html_len", len(params.Text),
		)
		params.Text = plainBody
		params.ParseMode = ""
		_, err = bot.SendMessage(ctx, params)
		if err == nil {
			return nil
		}
		errStr = err.Error()
	}

	if strings.Contains(errStr, "message is too long") {
		// One-level fallback: split this chunk in half on a best-effort
		// boundary and send each half independently. Prevents an unrecoverable
		// failure when our rune-based budget underestimates Telegram's
		// UTF-16 + entity accounting.
		slog.Warn("telegram: chunk still too long after pre-split, halving",
			"method", logMethod, "len", utf8.RuneCountInString(plainBody),
		)
		halves := splitForTelegram(plainBody, utf8.RuneCountInString(plainBody)/2-1)
		if len(halves) < 2 {
			return fmt.Errorf("telegram: %s: %w", logMethod, err)
		}
		for _, h := range halves {
			subParams := *params
			subParams.Text = core.MarkdownToSimpleHTML(h)
			subParams.ParseMode = models.ParseModeHTML
			// Reply params and markup must not be re-applied to recursive
			// children (already attached at outer level).
			subParams.ReplyParameters = nil
			subParams.ReplyMarkup = nil
			if e := sendOneChunkWithFallback(ctx, bot, &subParams, h, logMethod); e != nil {
				return e
			}
		}
		return nil
	}

	return fmt.Errorf("telegram: %s: %w", logMethod, err)
}
