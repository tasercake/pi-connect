package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tasercake/pi-connect/core"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func requireSourceReply(t *testing.T, params *models.ReplyParameters, wantMessageID int) {
	t.Helper()
	if params == nil || params.MessageID != wantMessageID {
		t.Fatalf("ReplyParameters = %#v, want message ID %d", params, wantMessageID)
	}
	if !params.AllowSendingWithoutReply {
		t.Fatalf("ReplyParameters.AllowSendingWithoutReply = false, want true")
	}
}

func TestTextResponsePathsReferenceSourceMessage(t *testing.T) {
	const sourceMessageID = 42
	rctx := replyContext{chatID: 100, threadID: 7, messageID: sourceMessageID}

	tests := []struct {
		name string
		run  func(*Platform) error
	}{
		{name: "Reply", run: func(p *Platform) error { return p.Reply(context.Background(), rctx, "reply") }},
		{name: "Send", run: func(p *Platform) error { return p.Send(context.Background(), rctx, "final or intermediate") }},
		{name: "SendWithButtons", run: func(p *Platform) error {
			return p.SendWithButtons(context.Background(), rctx, "question", [][]core.ButtonOption{{{Text: "Yes", Data: "yes"}}})
		}},
		{name: "SendPreviewStart", run: func(p *Platform) error {
			_, err := p.SendPreviewStart(context.Background(), rctx, "staging")
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot := newStubTelegramBot()
			p := &Platform{bot: bot}
			if err := tt.run(p); err != nil {
				t.Fatalf("send response: %v", err)
			}
			if len(bot.sendMessageParams) != 1 {
				t.Fatalf("SendMessage calls = %d, want 1", len(bot.sendMessageParams))
			}
			requireSourceReply(t, bot.sendMessageParams[0].ReplyParameters, sourceMessageID)
		})
	}
}

func TestEveryLongResponseChunkReferencesSourceMessage(t *testing.T) {
	bot := newStubTelegramBot()
	p := &Platform{bot: bot}
	if err := p.Send(context.Background(), replyContext{chatID: 100, messageID: 42}, strings.Repeat("chunk ", 1000)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(bot.sendMessageParams) < 2 {
		t.Fatalf("SendMessage calls = %d, want multiple chunks", len(bot.sendMessageParams))
	}
	for i, params := range bot.sendMessageParams {
		if params.ReplyParameters == nil || params.ReplyParameters.MessageID != 42 {
			t.Fatalf("chunk %d ReplyParameters = %#v, want message ID 42", i, params.ReplyParameters)
		}
	}
}

func TestAttachmentResponsePathsReferenceSourceMessage(t *testing.T) {
	const sourceMessageID = 42
	rctx := replyContext{chatID: 100, threadID: 7, messageID: sourceMessageID}
	bot := newStubTelegramBot()
	p := &Platform{bot: bot}

	if err := p.SendImage(context.Background(), rctx, core.ImageAttachment{Data: []byte("image")}); err != nil {
		t.Fatalf("SendImage: %v", err)
	}
	if err := p.SendFile(context.Background(), rctx, core.FileAttachment{Data: []byte("file")}); err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if err := p.sendVoice(context.Background(), rctx, []byte("voice"), "ogg"); err != nil {
		t.Fatalf("sendVoice: %v", err)
	}
	if err := p.sendAudio(context.Background(), rctx, []byte("audio"), "mp3"); err != nil {
		t.Fatalf("sendAudio: %v", err)
	}

	requireSourceReply(t, bot.sendPhotoParams[0].ReplyParameters, sourceMessageID)
	requireSourceReply(t, bot.sendDocumentParams[0].ReplyParameters, sourceMessageID)
	requireSourceReply(t, bot.sendVoiceParams[0].ReplyParameters, sourceMessageID)
	requireSourceReply(t, bot.sendAudioParams[0].ReplyParameters, sourceMessageID)
}

func TestGeneralForumResponsesReferenceOriginalMessage(t *testing.T) {
	bot := newStubTelegramBot()
	handled := make(chan *core.Message, 1)
	p := &Platform{
		groupReplyAll: true,
		bot:           bot,
		selfUser:      &models.User{ID: 99, Username: "mybot"},
		handler:       func(_ core.Platform, msg *core.Message) { handled <- msg },
	}

	p.handleMessage(context.Background(), generalForumMessage(42, "investigate this"))
	msg := <-handled
	rc := msg.ReplyCtx.(replyContext)
	if rc.threadID != 77 || rc.messageID != 42 {
		t.Fatalf("ReplyCtx = %#v, want new topic and original message ID", rc)
	}
	if _, err := p.SendPreviewStart(context.Background(), msg.ReplyCtx, "staging"); err != nil {
		t.Fatalf("SendPreviewStart: %v", err)
	}

	waitForTelegramTest(t, time.Second, func() bool { return bot.SendMessageCallCount() == 3 })
	bot.mu.Lock()
	paramsCopy := append([]*tgbot.SendMessageParams(nil), bot.sendMessageParams...)
	bot.mu.Unlock()
	foundStaging := false
	for _, params := range paramsCopy {
		requireSourceReply(t, params.ReplyParameters, 42)
		foundStaging = foundStaging || params.Text == "staging"
	}
	if !foundStaging {
		t.Fatal("staging message not sent")
	}
}

func TestReconstructMessageReplyCtxRestoresSourceMessage(t *testing.T) {
	p := &Platform{shareSessionInChannel: true}
	rctx, err := p.ReconstructMessageReplyCtx("telegram:-100123:77", "42")
	if err != nil {
		t.Fatalf("ReconstructMessageReplyCtx: %v", err)
	}
	rc := rctx.(replyContext)
	if rc.chatID != -100123 || rc.threadID != 77 || rc.messageID != 42 {
		t.Fatalf("reply context = %#v", rc)
	}
	if _, err := p.ReconstructMessageReplyCtx("telegram:-100123:77", "not-a-number"); err == nil {
		t.Fatal("invalid message ID accepted")
	}
}

func TestOversizeFallbackPreservesReplyOnEveryDeliveredChunk(t *testing.T) {
	bot := newStubTelegramBot()
	bot.sendMessageErrByCall = map[int]error{1: errors.New("Bad Request: message is too long")}
	reply := replyParameters(42)
	markup := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "OK", CallbackData: "ok"}}}}
	p := &Platform{bot: bot}

	err := p.sendChunked(context.Background(), bot, "first half. second half.", chunkSendOptions{
		chatID:      100,
		threadID:    7,
		replyTo:     reply,
		replyMarkup: markup,
		logMethod:   "test",
	})
	if err != nil {
		t.Fatalf("sendChunked: %v", err)
	}
	if len(bot.sendMessageParams) < 3 {
		t.Fatalf("SendMessage calls = %d, want failed outer plus multiple children", len(bot.sendMessageParams))
	}
	last := len(bot.sendMessageParams) - 1
	for i := 1; i <= last; i++ {
		requireSourceReply(t, bot.sendMessageParams[i].ReplyParameters, 42)
		if i < last && bot.sendMessageParams[i].ReplyMarkup != nil {
			t.Fatalf("child %d ReplyMarkup = %#v, want nil", i, bot.sendMessageParams[i].ReplyMarkup)
		}
	}
	if bot.sendMessageParams[last].ReplyMarkup == nil {
		t.Fatal("last child ReplyMarkup = nil, want buttons on final chunk")
	}
}
