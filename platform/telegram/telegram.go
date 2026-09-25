package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tasercake/pi-connect/core"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

var telegramConvertAudioToOpus = core.ConvertAudioToOpus

func init() {
	core.RegisterPlatform("telegram", New)
}

type replyContext struct {
	chatID    int64
	threadID  int
	messageID int
}

type mediaGroupKey struct {
	chatID       int64
	threadID     int
	userID       int64
	mediaGroupID string
}

type generalRouteKey struct {
	chatID    int64
	messageID int
}

type pendingMediaGroup struct {
	key      mediaGroupKey
	token    uint64
	items    []pendingMediaGroupItem
	seen     map[int]struct{}
	accepted bool
	timer    *time.Timer
}

type pendingMediaGroupItem struct {
	messageID  int
	message    *models.Message
	core       core.Message
	imageFile  string
	fileID     string
	fileMime   string
	fileName   string
	hasCaption bool
	directed   bool
}

// telegramBot abstracts the Telegram bot API methods for testability.
// *tgbot.Bot satisfies this interface.
type telegramBot interface {
	SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*models.Message, error)
	SendPhoto(ctx context.Context, params *tgbot.SendPhotoParams) (*models.Message, error)
	SendDocument(ctx context.Context, params *tgbot.SendDocumentParams) (*models.Message, error)
	SendVoice(ctx context.Context, params *tgbot.SendVoiceParams) (*models.Message, error)
	SendAudio(ctx context.Context, params *tgbot.SendAudioParams) (*models.Message, error)
	SendChatAction(ctx context.Context, params *tgbot.SendChatActionParams) (bool, error)
	EditMessageText(ctx context.Context, params *tgbot.EditMessageTextParams) (*models.Message, error)
	DeleteMessage(ctx context.Context, params *tgbot.DeleteMessageParams) (bool, error)
	AnswerCallbackQuery(ctx context.Context, params *tgbot.AnswerCallbackQueryParams) (bool, error)
	SetMyCommands(ctx context.Context, params *tgbot.SetMyCommandsParams) (bool, error)
	GetFile(ctx context.Context, params *tgbot.GetFileParams) (*models.File, error)
	FileDownloadLink(f *models.File) string
	SetMessageReaction(ctx context.Context, params *tgbot.SetMessageReactionParams) (bool, error)
	CreateForumTopic(ctx context.Context, params *tgbot.CreateForumTopicParams) (*models.ForumTopic, error)
	EditForumTopic(ctx context.Context, params *tgbot.EditForumTopicParams) (bool, error)
}

type backoffTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type typingTicker interface {
	C() <-chan time.Time
	Stop()
}

type retryCause int

const (
	retryCauseInitialConnectFailure retryCause = iota
	retryCauseReconnectFailure
	retryCauseConnectionLost
)

type retryLoopError struct {
	cause retryCause
	err   error
}

func (e *retryLoopError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *retryLoopError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

type stdlibBackoffTimer struct {
	*time.Timer
}

func (t *stdlibBackoffTimer) C() <-chan time.Time { return t.Timer.C }

type stdlibTypingTicker struct {
	*time.Ticker
}

func (t *stdlibTypingTicker) C() <-chan time.Time { return t.Ticker.C }

// botFactory creates a bot, returns it plus self user info and a blocking poll function.
type botFactory func(token string, onUpdate func(context.Context, *models.Update), httpClient *http.Client) (telegramBot, *models.User, func(context.Context), error)

type Platform struct {
	token                 string
	allowFrom             string
	groupReplyAll         bool
	shareSessionInChannel bool
	enableReactions       bool
	httpClient            *http.Client

	mu                    sync.RWMutex
	bot                   telegramBot
	selfUser              *models.User
	handler               core.MessageHandler
	lifecycleHandler      core.PlatformLifecycleHandler
	cancel                context.CancelFunc
	lifecycleCtx          context.Context
	stopping              bool
	generation            uint64
	unavailableNotified   bool
	everConnected         bool
	newBot                botFactory
	newBackoffTimer       func(time.Duration) backoffTimer
	newTypingTicker       func(time.Duration) typingTicker
	titleGenerator        func(context.Context, string) (string, error)
	messageRoutePreflight func(*core.Message) core.MessageRouteDisposition

	mediaGroupMu       sync.Mutex
	mediaGroupDebounce time.Duration
	mediaGroups        map[mediaGroupKey]*pendingMediaGroup
	sealedMediaGroups  map[mediaGroupKey]time.Time
	mediaGroupToken    uint64

	generalRouteMu sync.Mutex
	generalRoutes  map[generalRouteKey]time.Time
	titleSem       chan struct{}

	progressMu       sync.Mutex
	progressGate     chan struct{}
	progressNext     time.Time
	progressInterval time.Duration
}

const (
	initialReconnectBackoff   = time.Second
	maxReconnectBackoff       = 30 * time.Second
	stableConnectionWindow    = 10 * time.Second
	defaultMediaGroupDebounce = 1500 * time.Millisecond
	telegramAPITimeout        = 10 * time.Second
	generalRouteTTL           = 10 * time.Minute
	titleConcurrency          = 1
	telegramMessageLimit      = 4096
	telegramProgressInterval  = 5 * time.Second
)

func New(opts map[string]any) (core.Platform, error) {
	token, _ := opts["token"].(string)
	if token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}
	allowFrom, _ := opts["allow_from"].(string)
	core.CheckAllowFrom("telegram", allowFrom)

	// Build HTTP client with optional proxy support.
	// Timeout must exceed the server-side long-poll duration (pollTimeout − 1s = 59s)
	// to avoid the HTTP client racing with Telegram's response. 90s gives 30s headroom.
	httpClient := &http.Client{Timeout: 90 * time.Second}
	if proxyURL, _ := opts["proxy"].(string); proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("telegram: invalid proxy URL %q: %w", proxyURL, err)
		}
		proxyUser, _ := opts["proxy_username"].(string)
		proxyPass, _ := opts["proxy_password"].(string)
		if proxyUser != "" {
			u.User = url.UserPassword(proxyUser, proxyPass)
		}
		httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
		slog.Info("telegram: using proxy", "proxy", u.Host, "auth", proxyUser != "")
	}

	groupReplyAll, _ := opts["group_reply_all"].(bool)
	shareSessionInChannel, _ := opts["share_session_in_channel"].(bool)
	enableReactions, _ := opts["enable_reactions"].(bool)
	return &Platform{
		token: token, allowFrom: allowFrom, groupReplyAll: groupReplyAll,
		shareSessionInChannel: shareSessionInChannel, enableReactions: enableReactions,
		httpClient: httpClient, titleSem: make(chan struct{}, titleConcurrency),
	}, nil
}

func (p *Platform) Name() string { return "telegram" }

func (p *Platform) SetConversationTitleGenerator(generator func(context.Context, string) (string, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.titleGenerator = generator
}

func (p *Platform) conversationTitleGenerator() func(context.Context, string) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.titleGenerator
}

func (p *Platform) SetMessageRoutePreflight(preflight func(*core.Message) core.MessageRouteDisposition) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messageRoutePreflight = preflight
}

func (p *Platform) messageRouteDisposition(msg *core.Message) core.MessageRouteDisposition {
	p.mu.RLock()
	preflight := p.messageRoutePreflight
	p.mu.RUnlock()
	if preflight == nil {
		return core.MessageRouteDefault
	}
	return preflight(msg)
}

func (p *Platform) Start(handler core.MessageHandler) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopping {
		return fmt.Errorf("telegram: platform stopped")
	}
	if p.newBot == nil {
		p.newBot = defaultNewBot
	}
	if p.newBackoffTimer == nil {
		p.newBackoffTimer = func(d time.Duration) backoffTimer {
			return &stdlibBackoffTimer{Timer: time.NewTimer(d)}
		}
	}
	if p.newTypingTicker == nil {
		p.newTypingTicker = func(d time.Duration) typingTicker {
			return &stdlibTypingTicker{Ticker: time.NewTicker(d)}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.handler = handler
	p.cancel = cancel
	p.lifecycleCtx = ctx
	p.bot = nil
	p.selfUser = nil

	go p.connectLoop(ctx)
	return nil
}

func (p *Platform) SetLifecycleHandler(h core.PlatformLifecycleHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lifecycleHandler = h
}

func defaultNewBot(token string, onUpdate func(context.Context, *models.Update), httpClient *http.Client) (telegramBot, *models.User, func(context.Context), error) {
	handler := func(ctx context.Context, b *tgbot.Bot, update *models.Update) {
		onUpdate(ctx, update)
	}
	opts := []tgbot.Option{
		tgbot.WithDefaultHandler(handler),
		tgbot.WithNotAsyncHandlers(),
	}
	if httpClient != nil {
		opts = append(opts, tgbot.WithHTTPClient(60*time.Second, httpClient))
	}
	b, err := tgbot.New(token, opts...)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	me, err := b.GetMe(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("getMe: %w", err)
	}
	return b, me, b.Start, nil
}

func (p *Platform) connectLoop(ctx context.Context) {
	backoff := initialReconnectBackoff

	for {
		if ctx.Err() != nil || p.isStopping() {
			return
		}

		startedAt := time.Now()
		err := p.runConnection(ctx)
		if ctx.Err() != nil || p.isStopping() {
			return
		}

		wait := backoff
		if time.Since(startedAt) >= stableConnectionWindow {
			wait = initialReconnectBackoff
			backoff = initialReconnectBackoff
		} else if backoff < maxReconnectBackoff {
			backoff *= 2
			if backoff > maxReconnectBackoff {
				backoff = maxReconnectBackoff
			}
		}

		if err != nil {
			cause := retryCauseReconnectFailure
			if retryErr, ok := err.(*retryLoopError); ok {
				cause = retryErr.cause
			}
			slog.Warn(retryLogMessage(cause), "error", err, "backoff", wait)
			if cause == retryCauseInitialConnectFailure || cause == retryCauseReconnectFailure {
				p.notifyUnavailable(err)
			}
		}

		timer := p.makeBackoffTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
	}
}

func (p *Platform) runConnection(ctx context.Context) error {
	factory := p.getNewBot()
	b, me, startPoll, err := factory(p.token, p.processUpdate, p.httpClient)
	if err != nil {
		cause := retryCauseInitialConnectFailure
		if p.hasEverConnected() {
			cause = retryCauseReconnectFailure
		}
		return &retryLoopError{
			cause: cause,
			err:   fmt.Errorf("telegram: connect failed: %w", err),
		}
	}
	if ctx.Err() != nil || p.isStopping() {
		return nil
	}

	gen, ok := p.publishBot(b, me)
	if !ok {
		return nil
	}

	slog.Info("telegram: connected", "bot", me.Username)
	p.emitReady(gen)

	// Start polling — blocks until ctx is cancelled or connection drops.
	startPoll(ctx)

	p.clearBot(gen, b)
	return nil
}

func (p *Platform) processUpdate(ctx context.Context, update *models.Update) {
	if update.CallbackQuery != nil {
		p.handleCallbackQuery(ctx, update.CallbackQuery)
		return
	}

	if update.Message == nil {
		return
	}
	p.handleMessage(ctx, update.Message)
}

func (p *Platform) handleMessage(ctx context.Context, msg *models.Message) {
	msgTime := time.Unix(int64(msg.Date), 0)
	if core.IsOldMessage(msgTime) {
		slog.Debug("telegram: ignoring old message after restart", "date", msgTime)
		return
	}
	if msg.From == nil {
		return
	}

	userName := msg.From.Username
	if userName == "" {
		userName = strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
	}

	// Use MessageThreadID only when it meaningfully isolates a sub-session:
	//  - Forum groups (IsForum=true): Topics feature — thread ID is the topic ID.
	//  - Non-group chats (private, channel): thread ID is safe to use since
	//    there are no "reply threads" that would accidentally fragment sessions.
	// Regular groups (IsForum=false): thread replies produce a non-zero
	// MessageThreadID, but using it would split an existing session each time
	// a user replies to a specific message — so we ignore it there.
	isGroup := msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup
	threadID := 0
	if msg.Chat.IsForum || !isGroup {
		threadID = msg.MessageThreadID
	}
	if isGeneralForumMessage(msg) {
		threadID = 1
	}
	sessionKey := p.buildSessionKey(msg.Chat.ID, threadID, msg.From.ID)
	channelKey := buildChannelKey(msg.Chat.ID, threadID)

	userID := strconv.FormatInt(msg.From.ID, 10)
	if !core.AllowList(p.allowFrom, userID) {
		slog.Debug("telegram: message from unauthorized user", "user", userID)
		return
	}

	chatName := ""
	if isGroup {
		chatName = msg.Chat.Title
	}

	directed := true
	if isGroup && !p.groupReplyAll {
		slog.Debug("telegram: checking group message", "text", msg.Text, "is_command", isCommand(msg))
		directed = p.isDirectedAtBot(msg)
		if !directed && msg.MediaGroupID == "" {
			return
		}
	}

	rctx := replyContext{chatID: msg.Chat.ID, threadID: threadID, messageID: msg.ID}
	if p.enableReactions && !isGeneralForumMessage(msg) {
		go p.reactToMessage(ctx, msg.Chat.ID, msg.ID, "⚡")
	}
	botName := p.botUsername()

	if len(msg.Photo) > 0 {
		best := msg.Photo[len(msg.Photo)-1]
		caption := stripBotMention(msg.Caption, botName)
		base := core.Message{
			SessionKey: sessionKey, Platform: "telegram",
			UserID: userID, UserName: userName, ChatName: chatName,
			Content:    caption,
			MessageID:  strconv.Itoa(msg.ID),
			ChannelKey: channelKey,
			ReplyCtx:   rctx,
		}
		if msg.MediaGroupID != "" {
			p.enqueueMediaGroupItem(mediaGroupKey{chatID: msg.Chat.ID, threadID: threadID, userID: msg.From.ID, mediaGroupID: msg.MediaGroupID}, pendingMediaGroupItem{
				messageID:  msg.ID,
				message:    msg,
				core:       base,
				imageFile:  best.FileID,
				hasCaption: caption != "",
				directed:   directed,
			})
			return
		}
		imgData, err := p.downloadFile(ctx, best.FileID)
		if err != nil {
			slog.Error("telegram: download photo failed", "error", err)
			p.sendAttachmentDownloadFailure(ctx, msg)
			return
		}
		base.Images = []core.ImageAttachment{{MimeType: "image/jpeg", Data: imgData}}
		p.dispatchMessage(ctx, &base, msg)
		return
	}

	if msg.Voice != nil {
		slog.Debug("telegram: voice received", "user", userName, "duration", msg.Voice.Duration)
		audioData, err := p.downloadFile(ctx, msg.Voice.FileID)
		if err != nil {
			slog.Error("telegram: download voice failed", "error", err)
			p.sendAttachmentDownloadFailure(ctx, msg)
			return
		}
		p.dispatchMessage(ctx, &core.Message{
			SessionKey: sessionKey, Platform: "telegram",
			UserID: userID, UserName: userName, ChatName: chatName,
			MessageID:  strconv.Itoa(msg.ID),
			ChannelKey: channelKey,
			Audio: &core.AudioAttachment{
				MimeType: msg.Voice.MimeType,
				Data:     audioData,
				Format:   "ogg",
				Duration: msg.Voice.Duration,
			},
			ReplyCtx: rctx,
		}, msg)
		return
	}

	if msg.Audio != nil {
		slog.Debug("telegram: audio file received", "user", userName)
		audioData, err := p.downloadFile(ctx, msg.Audio.FileID)
		if err != nil {
			slog.Error("telegram: download audio failed", "error", err)
			p.sendAttachmentDownloadFailure(ctx, msg)
			return
		}
		format := "mp3"
		if msg.Audio.MimeType != "" {
			parts := strings.SplitN(msg.Audio.MimeType, "/", 2)
			if len(parts) == 2 {
				format = parts[1]
			}
		}
		p.dispatchMessage(ctx, &core.Message{
			SessionKey: sessionKey, Platform: "telegram",
			UserID: userID, UserName: userName, ChatName: chatName,
			MessageID:  strconv.Itoa(msg.ID),
			ChannelKey: channelKey,
			Audio: &core.AudioAttachment{
				MimeType: msg.Audio.MimeType,
				Data:     audioData,
				Format:   format,
				Duration: msg.Audio.Duration,
			},
			ReplyCtx: rctx,
		}, msg)
		return
	}

	if msg.Document != nil {
		slog.Info("telegram: document received", "user", userName, "file_name", msg.Document.FileName, "mime", msg.Document.MimeType, "file_id", msg.Document.FileID)
		caption := stripBotMention(msg.Caption, botName)
		base := core.Message{
			SessionKey: sessionKey, Platform: "telegram",
			UserID: userID, UserName: userName, ChatName: chatName,
			Content:    caption,
			MessageID:  strconv.Itoa(msg.ID),
			ChannelKey: channelKey,
			ReplyCtx:   rctx,
		}
		if msg.MediaGroupID != "" {
			p.enqueueMediaGroupItem(mediaGroupKey{chatID: msg.Chat.ID, threadID: threadID, userID: msg.From.ID, mediaGroupID: msg.MediaGroupID}, pendingMediaGroupItem{
				messageID:  msg.ID,
				message:    msg,
				core:       base,
				fileID:     msg.Document.FileID,
				fileMime:   msg.Document.MimeType,
				fileName:   msg.Document.FileName,
				hasCaption: caption != "",
				directed:   directed,
			})
			return
		}
		fileData, err := p.downloadFile(ctx, msg.Document.FileID)
		if err != nil {
			slog.Error("telegram: download document failed", "error", err)
			p.sendAttachmentDownloadFailure(ctx, msg)
			return
		}
		base.Files = []core.FileAttachment{{MimeType: msg.Document.MimeType, Data: fileData, FileName: msg.Document.FileName}}
		p.dispatchMessage(ctx, &base, msg)
		return
	}

	if msg.Location != nil {
		slog.Info("telegram: location received", "user", userName, "latitude", msg.Location.Latitude, "longitude", msg.Location.Longitude)
		p.dispatchMessage(ctx, &core.Message{
			SessionKey: sessionKey, Platform: "telegram",
			UserID: userID, UserName: userName, ChatName: chatName,
			MessageID:  strconv.Itoa(msg.ID),
			ChannelKey: channelKey,
			Location: &core.LocationAttachment{
				Latitude:             msg.Location.Latitude,
				Longitude:            msg.Location.Longitude,
				HorizontalAccuracy:   msg.Location.HorizontalAccuracy,
				LivePeriod:           msg.Location.LivePeriod,
				Heading:              msg.Location.Heading,
				ProximityAlertRadius: msg.Location.ProximityAlertRadius,
			},
			ReplyCtx: rctx,
		}, msg)
		return
	}
	if msg.Text == "" {
		return
	}

	text := stripBotMention(msg.Text, botName)
	slog.Debug("telegram: message received", "user", userName, "chat", msg.Chat.ID)
	p.dispatchMessage(ctx, &core.Message{
		SessionKey: sessionKey, Platform: "telegram",
		UserID: userID, UserName: userName, ChatName: chatName,
		Content:    text,
		MessageID:  strconv.Itoa(msg.ID),
		ChannelKey: channelKey,
		ReplyCtx:   rctx,
	}, msg)
}

func (p *Platform) enqueueMediaGroupItem(key mediaGroupKey, item pendingMediaGroupItem) {
	debounce := p.mediaGroupDebounce
	if debounce <= 0 {
		debounce = defaultMediaGroupDebounce
	}

	p.mediaGroupMu.Lock()
	now := time.Now()
	for sealedKey, expires := range p.sealedMediaGroups {
		if !expires.After(now) {
			delete(p.sealedMediaGroups, sealedKey)
		}
	}
	if expires := p.sealedMediaGroups[key]; expires.After(now) {
		p.mediaGroupMu.Unlock()
		return
	}
	if p.mediaGroups == nil {
		p.mediaGroups = make(map[mediaGroupKey]*pendingMediaGroup)
	}
	group := p.mediaGroups[key]
	if group == nil {
		p.mediaGroupToken++
		group = &pendingMediaGroup{key: key, token: p.mediaGroupToken, seen: make(map[int]struct{})}
		p.mediaGroups[key] = group
	}
	if _, duplicate := group.seen[item.messageID]; duplicate {
		p.mediaGroupMu.Unlock()
		return
	}
	group.seen[item.messageID] = struct{}{}
	group.items = append(group.items, item)
	group.accepted = group.accepted || item.directed
	if group.timer != nil {
		group.timer.Stop()
	}
	token := group.token
	group.timer = time.AfterFunc(debounce, func() {
		p.flushMediaGroup(key, token)
	})
	p.mediaGroupMu.Unlock()
}

func (p *Platform) flushMediaGroup(key mediaGroupKey, token uint64) {
	p.mediaGroupMu.Lock()
	group := p.mediaGroups[key]
	if group == nil || group.token != token {
		p.mediaGroupMu.Unlock()
		return
	}
	delete(p.mediaGroups, key)
	if p.sealedMediaGroups == nil {
		p.sealedMediaGroups = make(map[mediaGroupKey]time.Time)
	}
	expires := time.Now().Add(generalRouteTTL)
	p.sealedMediaGroups[key] = expires
	time.AfterFunc(generalRouteTTL, func() {
		p.mediaGroupMu.Lock()
		if p.sealedMediaGroups[key].Equal(expires) {
			delete(p.sealedMediaGroups, key)
		}
		p.mediaGroupMu.Unlock()
	})
	accepted := group.accepted
	items := append([]pendingMediaGroupItem(nil), group.items...)
	p.mediaGroupMu.Unlock()

	if !accepted || len(items) == 0 || p.isStopping() {
		return
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].messageID < items[j].messageID })

	baseIdx := 0
	for i, item := range items {
		if item.hasCaption {
			baseIdx = i
			break
		}
	}

	out := items[baseIdx].core
	out.Images = nil
	out.Files = nil
	out.MessageID = strconv.Itoa(items[0].messageID)
	ctx, cancel := p.operationContext(context.Background(), telegramAPITimeout)
	defer cancel()
	for _, item := range items {
		if item.imageFile != "" {
			imgData, err := p.downloadFile(ctx, item.imageFile)
			if err != nil {
				slog.Error("telegram: download media group photo failed", "error", err, "message_id", item.messageID)
				p.sendAttachmentDownloadFailure(ctx, items[baseIdx].message)
				return
			}
			out.Images = append(out.Images, core.ImageAttachment{MimeType: "image/jpeg", Data: imgData})
			continue
		}
		if item.fileID != "" {
			fileData, err := p.downloadFile(ctx, item.fileID)
			if err != nil {
				slog.Error("telegram: download media group document failed", "error", err, "message_id", item.messageID)
				p.sendAttachmentDownloadFailure(ctx, items[baseIdx].message)
				return
			}
			out.Files = append(out.Files, core.FileAttachment{MimeType: item.fileMime, Data: fileData, FileName: item.fileName})
		}
	}

	p.dispatchMessage(ctx, &out, items[baseIdx].message)
}

func (p *Platform) cancelMediaGroups() {
	p.mediaGroupMu.Lock()
	defer p.mediaGroupMu.Unlock()
	for key, group := range p.mediaGroups {
		if group.timer != nil {
			group.timer.Stop()
		}
		delete(p.mediaGroups, key)
	}
	clear(p.sealedMediaGroups)
}

const telegramForumTopicNameLimit = 128

// routeGeneralForumMessage creates a fallback-named topic synchronously so the
// first core dispatch uses final identity. Returned function starts source
// delivery; optional title decoration waits for core dispatch completion.
func (p *Platform) routeGeneralForumMessage(ctx context.Context, msg *core.Message, tgMsg *models.Message) (func(<-chan struct{}), bool) {
	if !isGeneralForumMessage(tgMsg) {
		return nil, true
	}
	if !p.claimGeneralRoute(tgMsg.Chat.ID, tgMsg.ID) {
		slog.Debug("telegram: duplicate General message ignored", "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID)
		return nil, false
	}
	if p.messageRouteDisposition(msg) == core.MessageRouteInPlace {
		msg.Sessionless = true
		return nil, true
	}

	bot, err := p.connectedBot("create forum topic")
	if err != nil {
		slog.Error("telegram: prepare forum topic failed", "error", err, "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID)
		return nil, false
	}
	fallbackName := fallbackForumTopicName(tgMsg)
	topic, err := p.createForumTopic(ctx, bot, tgMsg.Chat.ID, fallbackName)
	if err != nil || topic == nil || topic.MessageThreadID <= 1 {
		if err == nil {
			err = fmt.Errorf("invalid topic response")
		}
		slog.Error("telegram: create forum topic failed", "error", err, "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID)
		failureCtx, cancel := p.operationContext(ctx, telegramAPITimeout)
		p.sendTopicCreationFailure(failureCtx, bot, tgMsg)
		cancel()
		return nil, false
	}

	threadID := topic.MessageThreadID
	sourceChannelKey := buildChannelKey(tgMsg.Chat.ID, 1)
	msg.SessionKey = p.buildSessionKey(tgMsg.Chat.ID, threadID, tgMsg.From.ID)
	msg.ChannelKey = buildChannelKey(tgMsg.Chat.ID, threadID)
	msg.WorkspaceSourceChannelKey = sourceChannelKey
	msg.ReplyCtx = replyContext{chatID: tgMsg.Chat.ID, threadID: threadID, messageID: tgMsg.ID}
	titleInput := forumTopicNamingInput(msg, tgMsg)

	return func(dispatchDone <-chan struct{}) {
		go p.finishGeneralRoute(bot, tgMsg, threadID, titleInput, dispatchDone)
	}, true
}

func (p *Platform) claimGeneralRoute(chatID int64, messageID int) bool {
	p.generalRouteMu.Lock()
	defer p.generalRouteMu.Unlock()
	if p.generalRoutes == nil {
		p.generalRoutes = make(map[generalRouteKey]time.Time)
	}
	now := time.Now()
	for key, expires := range p.generalRoutes {
		if !expires.After(now) {
			delete(p.generalRoutes, key)
		}
	}
	key := generalRouteKey{chatID: chatID, messageID: messageID}
	if p.generalRoutes[key].After(now) {
		return false
	}
	expires := now.Add(generalRouteTTL)
	p.generalRoutes[key] = expires
	time.AfterFunc(generalRouteTTL, func() {
		p.generalRouteMu.Lock()
		if p.generalRoutes[key].Equal(expires) {
			delete(p.generalRoutes, key)
		}
		p.generalRouteMu.Unlock()
	})
	return true
}

func fallbackForumTopicName(msg *models.Message) string {
	base := core.NewI18n(telegramMessageLanguage(strings.TrimSpace(msg.Text+" "+msg.Caption), msg)).T(core.MsgForumTopicNewRequest)
	name := fmt.Sprintf("%s · %d", base, msg.ID)
	runes := []rune(name)
	if len(runes) > telegramForumTopicNameLimit {
		name = string(runes[:telegramForumTopicNameLimit])
	}
	return name
}

func (p *Platform) createForumTopic(ctx context.Context, bot telegramBot, chatID int64, name string) (*models.ForumTopic, error) {
	for attempt := 0; attempt < 2; attempt++ {
		callCtx, cancel := p.operationContext(ctx, telegramAPITimeout)
		topic, err := bot.CreateForumTopic(callCtx, &tgbot.CreateForumTopicParams{ChatID: chatID, Name: name})
		cancel()
		if err == nil {
			return topic, nil
		}
		var rateErr *tgbot.TooManyRequestsError
		if !errors.As(err, &rateErr) || attempt > 0 || rateErr.RetryAfter < 0 || rateErr.RetryAfter > 2 {
			return nil, err
		}
		timer := time.NewTimer(time.Duration(rateErr.RetryAfter) * time.Second)
		base := p.operationBaseContext(ctx)
		select {
		case <-base.Done():
			timer.Stop()
			return nil, base.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("topic creation retry exhausted")
}

func (p *Platform) finishGeneralRoute(bot telegramBot, tgMsg *models.Message, threadID int, titleInput string, dispatchDone <-chan struct{}) {
	handoffCtx, handoffCancel := p.operationContext(context.Background(), telegramAPITimeout)
	_, err := bot.SendMessage(handoffCtx, &tgbot.SendMessageParams{
		ChatID:             tgMsg.Chat.ID,
		Text:               telegramMessageLink(tgMsg.Chat, threadID),
		ReplyParameters:    replyParameters(tgMsg.ID),
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tgbot.True()},
	})
	handoffCancel()
	if err != nil {
		slog.Warn("telegram: send forum topic handoff failed", "error", err, "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID, "thread_id", threadID)
	}

	select {
	case <-dispatchDone:
	case <-p.operationBaseContext(context.Background()).Done():
		return
	}
	p.renameForumTopic(bot, tgMsg, threadID, titleInput)
}

func (p *Platform) renameForumTopic(bot telegramBot, tgMsg *models.Message, threadID int, titleInput string) {
	generator := p.conversationTitleGenerator()
	if generator == nil {
		slog.Debug("telegram: optional forum topic rename skipped; generator unavailable", "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID)
		return
	}
	sem := p.titleSemaphore()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		slog.Warn("telegram: optional forum topic rename dropped; title worker saturated", "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID)
		return
	}

	titleCtx, cancel := p.operationContext(context.Background(), 2*time.Minute)
	name, err := generator(titleCtx, titleInput)
	cancel()
	if err == nil {
		name, err = normalizeGeneratedForumTopicName(name)
	}
	if err != nil {
		slog.Warn("telegram: optional forum topic rename failed", "error", err, "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID)
		return
	}
	editCtx, editCancel := p.operationContext(context.Background(), telegramAPITimeout)
	_, err = bot.EditForumTopic(editCtx, &tgbot.EditForumTopicParams{ChatID: tgMsg.Chat.ID, MessageThreadID: threadID, Name: name})
	editCancel()
	if err != nil {
		slog.Warn("telegram: edit forum topic failed; keeping fallback title", "error", err, "chat_id", tgMsg.Chat.ID, "message_id", tgMsg.ID, "thread_id", threadID)
	}
}

func (p *Platform) titleSemaphore() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.titleSem == nil {
		p.titleSem = make(chan struct{}, titleConcurrency)
	}
	return p.titleSem
}

func isGeneralForumMessage(msg *models.Message) bool {
	if msg == nil || !msg.Chat.IsForum || msg.Chat.Type != models.ChatTypeSupergroup {
		return false
	}
	return msg.MessageThreadID == 0 || msg.MessageThreadID == 1
}

func forumTopicNamingInput(msg *core.Message, tgMsg *models.Message) string {
	input := strings.Join(strings.Fields(strings.TrimSpace(msg.ExtraContent+"\n"+msg.Content)), " ")
	if input != "" {
		return input
	}

	i18n := core.NewI18n(telegramMessageLanguage(msg.Content, tgMsg))
	switch {
	case len(msg.Images) > 1:
		return i18n.T(core.MsgForumTopicPhotos)
	case len(msg.Images) == 1:
		return i18n.T(core.MsgForumTopicPhoto)
	case len(msg.Files) > 0 && msg.Files[0].FileName != "":
		return i18n.T(core.MsgForumTopicDocument) + ": " + msg.Files[0].FileName
	case len(msg.Files) > 0 || tgMsg.Document != nil:
		return i18n.T(core.MsgForumTopicDocument)
	case msg.Audio != nil:
		return i18n.T(core.MsgForumTopicAudio)
	case msg.Location != nil:
		return i18n.T(core.MsgForumTopicLocation)
	default:
		return i18n.T(core.MsgForumTopicNewRequest)
	}
}

func normalizeGeneratedForumTopicName(raw string) (string, error) {
	var name string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			name = line
			break
		}
	}
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
	name = strings.Trim(name, " \t\"'`*_#")
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", fmt.Errorf("generated topic name is empty")
	}
	lower := strings.ToLower(name)
	for _, prefix := range []string{"sure", "i can", "i'll", "i will", "here is", "you should", "happy to", "of course"} {
		if lower == prefix || strings.HasPrefix(lower, prefix+" ") || strings.HasPrefix(lower, prefix+",") || strings.HasPrefix(lower, prefix+":") {
			return "", fmt.Errorf("generated topic name is conversational")
		}
	}

	runes := []rune(name)
	if len(runes) > telegramForumTopicNameLimit {
		name = string(runes[:telegramForumTopicNameLimit-1]) + "…"
	}
	return name, nil
}

func telegramMessageLanguage(content string, tgMsg *models.Message) core.Language {
	if strings.TrimSpace(content) != "" {
		return core.DetectLanguage(content)
	}
	if tgMsg == nil || tgMsg.From == nil {
		return core.LangEnglish
	}

	code := strings.ToLower(strings.ReplaceAll(tgMsg.From.LanguageCode, "_", "-"))
	switch {
	case strings.HasPrefix(code, "zh-tw"), strings.HasPrefix(code, "zh-hk"), strings.HasPrefix(code, "zh-mo"), strings.Contains(code, "hant"):
		return core.LangTraditionalChinese
	case strings.HasPrefix(code, "zh"):
		return core.LangChinese
	case strings.HasPrefix(code, "ja"):
		return core.LangJapanese
	case strings.HasPrefix(code, "es"):
		return core.LangSpanish
	default:
		return core.LangEnglish
	}
}

func telegramMessageLink(chat models.Chat, messageID int) string {
	if chat.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", strings.TrimPrefix(chat.Username, "@"), messageID)
	}
	internalID := strings.TrimPrefix(strconv.FormatInt(chat.ID, 10), "-100")
	return fmt.Sprintf("https://t.me/c/%s/%d", internalID, messageID)
}

func (p *Platform) sendTopicCreationFailure(ctx context.Context, bot telegramBot, msg *models.Message) {
	p.sendTopicFailure(ctx, bot, msg, core.MsgForumTopicCreationFailed)
}

func (p *Platform) sendAttachmentDownloadFailure(ctx context.Context, msg *models.Message) {
	if !isGeneralForumMessage(msg) {
		return
	}
	bot, err := p.connectedBot("report attachment download failure")
	if err != nil {
		return
	}
	failureCtx, cancel := p.operationContext(ctx, telegramAPITimeout)
	defer cancel()
	p.sendTopicFailure(failureCtx, bot, msg, core.MsgForumTopicAttachmentFailed)
}

func (p *Platform) sendTopicFailure(ctx context.Context, bot telegramBot, msg *models.Message, key core.MsgKey) {
	if _, err := bot.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:          msg.Chat.ID,
		Text:            core.NewI18n(telegramMessageLanguage(strings.TrimSpace(msg.Text+" "+msg.Caption), msg)).T(key),
		ReplyParameters: replyParameters(msg.ID),
	}); err != nil {
		slog.Error("telegram: send topic failure failed", "error", err, "chat_id", msg.Chat.ID, "message_id", msg.ID)
	}
}

func (p *Platform) dispatchMessage(ctx context.Context, msg *core.Message, tgMsg *models.Message) {
	// Enrich with platform-specific context (reply quotes, location text, etc.)
	var extras []string
	if replyText := enrichReplyContent(tgMsg); replyText != "" {
		extras = append(extras, replyText)
	}
	if locText := enrichLocation(msg); locText != "" {
		extras = append(extras, locText)
	}
	if len(extras) > 0 {
		msg.ExtraContent = strings.Join(extras, "\n")
	}

	postDispatch, ok := p.routeGeneralForumMessage(ctx, msg, tgMsg)
	if !ok {
		return
	}
	handler := p.messageHandler()
	if postDispatch == nil {
		if handler != nil {
			handler(p, msg)
		}
		return
	}

	// Start the General-to-topic handoff before dispatch. Optional title work
	// waits until core dispatch returns.
	dispatchDone := make(chan struct{})
	postDispatch(dispatchDone)
	if handler != nil {
		handler(p, msg)
	}
	close(dispatchDone)
}

func (p *Platform) messageHandler() core.MessageHandler {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.handler
}

// reactToMessage sets an emoji reaction on a Telegram message.
// It is called asynchronously so it never blocks the message dispatch path.
func (p *Platform) reactToMessage(ctx context.Context, chatID int64, messageID int, emoji string) {
	bot, err := p.connectedBot("react")
	if err != nil {
		return
	}
	if _, err := bot.SetMessageReaction(ctx, &tgbot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: messageID,
		Reaction: []models.ReactionType{{
			Type:              models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji},
		}},
	}); err != nil {
		slog.Debug("telegram: set reaction failed", "error", err)
	}
}

func (p *Platform) buildSessionKey(chatID int64, threadID int, userID int64) string {
	if p.shareSessionInChannel {
		if threadID != 0 {
			return fmt.Sprintf("telegram:%d:%d", chatID, threadID)
		}
		return fmt.Sprintf("telegram:%d", chatID)
	}
	if threadID != 0 {
		return fmt.Sprintf("telegram:%d:%d:%d", chatID, threadID, userID)
	}
	return fmt.Sprintf("telegram:%d:%d", chatID, userID)
}

func buildChannelKey(chatID int64, threadID int) string {
	if threadID != 0 {
		return fmt.Sprintf("%d:%d", chatID, threadID)
	}
	return strconv.FormatInt(chatID, 10)
}

func stripBotMention(text, botName string) string {
	if botName == "" {
		return text
	}
	text = strings.ReplaceAll(text, "@"+botName, "")
	return strings.TrimSpace(text)
}

func (p *Platform) getNewBot() botFactory {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.newBot
}

func (p *Platform) makeBackoffTimer(d time.Duration) backoffTimer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.newBackoffTimer(d)
}

func (p *Platform) isStopping() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stopping
}

func (p *Platform) publishBot(b telegramBot, me *models.User) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopping {
		return 0, false
	}
	p.generation++
	p.bot = b
	p.selfUser = me
	return p.generation, true
}

func (p *Platform) emitReady(gen uint64) {
	p.mu.RLock()
	if p.stopping || p.generation != gen || p.bot == nil {
		p.mu.RUnlock()
		return
	}
	handler := p.lifecycleHandler
	p.mu.RUnlock()
	p.markReady()

	if handler != nil {
		handler.OnPlatformReady(p)
	}
}

func (p *Platform) clearBot(gen uint64, b telegramBot) {
	notify := false
	p.mu.Lock()
	if p.bot == b && p.generation == gen {
		p.bot = nil
		p.selfUser = nil
		notify = !p.stopping
	}
	p.mu.Unlock()

	if notify {
		p.notifyUnavailable(fmt.Errorf("telegram: connection lost"))
	}
}

func (p *Platform) connectedBot(action string) (telegramBot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.bot == nil {
		return nil, fmt.Errorf("telegram: %s: bot not connected", action)
	}
	return p.bot, nil
}

func (p *Platform) operationBaseContext(fallback context.Context) context.Context {
	p.mu.RLock()
	base := p.lifecycleCtx
	p.mu.RUnlock()
	if base != nil {
		return base
	}
	if fallback != nil {
		return context.WithoutCancel(fallback)
	}
	return context.Background()
}

func (p *Platform) operationContext(fallback context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(p.operationBaseContext(fallback), timeout)
}

func (p *Platform) botUsername() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.selfUser == nil {
		return ""
	}
	return p.selfUser.Username
}

func (p *Platform) hasEverConnected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.everConnected
}

func (p *Platform) markReady() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.everConnected = true
	p.unavailableNotified = false
}

func (p *Platform) notifyUnavailable(err error) {
	var handler core.PlatformLifecycleHandler

	p.mu.Lock()
	if p.stopping || err == nil || p.unavailableNotified {
		p.mu.Unlock()
		return
	}
	p.unavailableNotified = true
	handler = p.lifecycleHandler
	p.mu.Unlock()

	if handler != nil {
		handler.OnPlatformUnavailable(p, err)
	}
}

func retryLogMessage(cause retryCause) string {
	switch cause {
	case retryCauseInitialConnectFailure:
		return "telegram: initial connection failed, retrying"
	case retryCauseConnectionLost:
		return "telegram: connection lost, retrying"
	default:
		return "telegram: reconnect failed, retrying"
	}
}

func (p *Platform) handleCallbackQuery(ctx context.Context, cb *models.CallbackQuery) {
	msg := cb.Message.Message
	if msg == nil {
		return
	}

	bot, err := p.connectedBot("callback query")
	if err != nil {
		slog.Debug("telegram: ignoring callback for disconnected bot", "error", err)
		return
	}

	data := cb.Data
	chatID := msg.Chat.ID
	msgID := msg.ID
	userID := strconv.FormatInt(cb.From.ID, 10)

	if !core.AllowList(p.allowFrom, userID) {
		slog.Debug("telegram: callback from unauthorized user", "user", userID)
		return
	}

	// Answer the callback to clear the loading indicator
	if _, err := bot.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID}); err != nil {
		slog.Debug("telegram: answer callback failed", "error", err)
	}

	userName := cb.From.Username
	if userName == "" {
		userName = strings.TrimSpace(cb.From.FirstName + " " + cb.From.LastName)
	}

	isGroupChat := msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup
	threadID := 0
	if msg.Chat.IsForum || !isGroupChat {
		threadID = msg.MessageThreadID
	}
	generalForum := isGeneralForumMessage(msg)
	if generalForum {
		threadID = 1
	}
	sessionKey := p.buildSessionKey(chatID, threadID, cb.From.ID)
	channelKey := buildChannelKey(chatID, threadID)

	isGroup := isGroupChat
	chatName := ""
	if isGroup {
		chatName = msg.Chat.Title
	}
	rctx := replyContext{chatID: chatID, threadID: threadID, messageID: msgID}

	emptyMarkup := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}

	// Command callbacks (cmd:/lang en, cmd:/mode yolo, etc.)
	if strings.HasPrefix(data, "cmd:") {
		command := strings.TrimPrefix(data, "cmd:")

		origText := msg.Text
		if origText == "" {
			origText = ""
		}
		if _, err := bot.EditMessageText(ctx, &tgbot.EditMessageTextParams{
			ChatID:      chatID,
			MessageID:   msgID,
			Text:        origText + "\n\n> " + command,
			ReplyMarkup: emptyMarkup,
		}); err != nil {
			slog.Debug("telegram: callback edit failed", "error", err)
		}

		commandMsg := &core.Message{
			SessionKey: sessionKey,
			Platform:   "telegram",
			UserID:     userID,
			UserName:   userName,
			ChatName:   chatName,
			Content:    command,
			MessageID:  strconv.Itoa(msgID),
			ChannelKey: channelKey,
			ReplyCtx:   rctx,
		}
		if generalForum && p.messageRouteDisposition(commandMsg) == core.MessageRouteInPlace {
			commandMsg.Sessionless = true
		}
		p.handler(p, commandMsg)
		return
	}

	// AskUserQuestion callbacks (askq:qIdx:optIdx)
	if strings.HasPrefix(data, "askq:") {
		parts := strings.SplitN(data, ":", 3)
		choiceLabel := data
		if len(parts) == 3 {
			if msg.ReplyMarkup != nil {
				for _, row := range msg.ReplyMarkup.InlineKeyboard {
					for _, btn := range row {
						if btn.CallbackData == data {
							choiceLabel = "✅ " + btn.Text
						}
					}
				}
			}
		}

		origText := msg.Text
		if origText == "" {
			origText = "(question)"
		}
		if _, err := bot.EditMessageText(ctx, &tgbot.EditMessageTextParams{
			ChatID:      chatID,
			MessageID:   msgID,
			Text:        origText + "\n\n" + choiceLabel,
			ReplyMarkup: emptyMarkup,
		}); err != nil {
			slog.Debug("telegram: callback edit failed", "error", err)
		}

		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "telegram",
			UserID:     userID,
			UserName:   userName,
			ChatName:   chatName,
			Content:    data,
			MessageID:  strconv.Itoa(msgID),
			ChannelKey: channelKey,
			ReplyCtx:   rctx,
		})
		return
	}

	// Permission callbacks (perm:allow, perm:deny, perm:allow_all)
	var responseText string
	switch data {
	case "perm:allow":
		responseText = "allow"
	case "perm:deny":
		responseText = "deny"
	case "perm:allow_all":
		responseText = "allow all"
	default:
		slog.Debug("telegram: unknown callback data", "data", data)
		return
	}

	choiceLabel := responseText
	switch data {
	case "perm:allow":
		choiceLabel = "✅ Allowed"
	case "perm:deny":
		choiceLabel = "❌ Denied"
	case "perm:allow_all":
		choiceLabel = "✅ Allow All"
	}

	origText := msg.Text
	if origText == "" {
		origText = "(permission request)"
	}
	if _, err := bot.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   msgID,
		Text:        origText + "\n\n" + choiceLabel,
		ReplyMarkup: emptyMarkup,
	}); err != nil {
		slog.Debug("telegram: permission callback edit failed", "error", err)
	}

	p.handler(p, &core.Message{
		SessionKey: sessionKey,
		Platform:   "telegram",
		UserID:     userID,
		UserName:   userName,
		ChatName:   chatName,
		Content:    responseText,
		MessageID:  strconv.Itoa(msgID),
		ChannelKey: channelKey,
		ReplyCtx:   rctx,
	})
}

// isDirectedAtBot checks whether a group message is directed at this bot:
//   - Command with @thisbot suffix (e.g. /help@thisbot)
//   - Command without @suffix (broadcast to all bots — accept it)
//   - Command with @otherbot suffix → reject
//   - Non-command: accept if bot is @mentioned or message is a reply to bot
func (p *Platform) isDirectedAtBot(msg *models.Message) bool {
	p.mu.RLock()
	self := p.selfUser
	p.mu.RUnlock()
	if self == nil {
		slog.Debug("telegram: ignoring group routing, self user unknown")
		return false
	}
	botName := self.Username

	// Commands: /cmd or /cmd@botname
	if isCommand(msg) {
		atIdx := strings.Index(msg.Text, "@")
		spaceIdx := strings.Index(msg.Text, " ")
		cmdEnd := len(msg.Text)
		if spaceIdx > 0 {
			cmdEnd = spaceIdx
		}
		if atIdx > 0 && atIdx < cmdEnd {
			target := msg.Text[atIdx+1 : cmdEnd]
			slog.Debug("telegram: command with @suffix", "bot", botName, "target", target, "match", strings.EqualFold(target, botName))
			return strings.EqualFold(target, botName)
		}
		slog.Debug("telegram: command without @suffix, accepting", "bot", botName, "text", msg.Text)
		return true // /cmd without @suffix — accept
	}

	// Non-command: check @mention
	if msg.Entities != nil {
		for _, e := range msg.Entities {
			if e.Type == models.MessageEntityTypeMention {
				mention := extractEntityText(msg.Text, e.Offset, e.Length)
				slog.Debug("telegram: checking mention", "bot", botName, "mention", mention, "match", strings.EqualFold(mention, "@"+botName))
				if strings.EqualFold(mention, "@"+botName) {
					return true
				}
			}
		}
	}

	// Check if replying to a message from this bot
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		slog.Debug("telegram: checking reply", "bot_id", self.ID, "reply_from_id", msg.ReplyToMessage.From.ID)
		if msg.ReplyToMessage.From.ID == self.ID {
			return true
		}
	}

	// Also check caption entities (for photos with captions)
	if msg.CaptionEntities != nil {
		for _, e := range msg.CaptionEntities {
			if e.Type == models.MessageEntityTypeMention {
				mention := extractEntityText(msg.Caption, e.Offset, e.Length)
				if strings.EqualFold(mention, "@"+botName) {
					return true
				}
			}
		}
	}

	slog.Debug("telegram: ignoring group message not directed at bot", "chat", msg.Chat.ID, "bot", botName, "text", msg.Text, "entities", msg.Entities)
	return false
}

func isCommand(msg *models.Message) bool {
	for _, e := range msg.Entities {
		if e.Type == models.MessageEntityTypeBotCommand && e.Offset == 0 {
			return true
		}
	}
	return false
}

func resolveReplyContext(_ context.Context, rctx any) (replyContext, error) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return replyContext{}, fmt.Errorf("telegram: invalid reply context type %T", rctx)
	}
	return rc, nil
}

// replyParameters keeps delivery independent from the source message lifetime.
// Telegram sends the message normally when its reply target was deleted.
func replyParameters(messageID int) *models.ReplyParameters {
	if messageID == 0 {
		return nil
	}
	return &models.ReplyParameters{
		MessageID:                messageID,
		AllowSendingWithoutReply: true,
	}
}

func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, err := resolveReplyContext(ctx, rctx)
	if err != nil {
		return err
	}
	bot, err := p.connectedBot("reply")
	if err != nil {
		return err
	}
	return p.sendChunked(ctx, bot, content, chunkSendOptions{
		chatID:       rc.chatID,
		threadID:     rc.threadID,
		replyTo:      replyParameters(rc.messageID),
		logMethod:    "Reply",
		logChunkInfo: true,
	})
}

// Send sends a response associated with the incoming message when available.
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	rc, err := resolveReplyContext(ctx, rctx)
	if err != nil {
		return err
	}
	bot, err := p.connectedBot("send")
	if err != nil {
		return err
	}
	return p.sendChunked(ctx, bot, content, chunkSendOptions{
		chatID:       rc.chatID,
		threadID:     rc.threadID,
		replyTo:      replyParameters(rc.messageID),
		logMethod:    "Send",
		logChunkInfo: true,
	})
}

func (p *Platform) SendImage(ctx context.Context, rctx any, img core.ImageAttachment) error {
	rc, err := resolveReplyContext(ctx, rctx)
	if err != nil {
		return err
	}
	bot, err := p.connectedBot("send image")
	if err != nil {
		return err
	}

	name := img.FileName
	if name == "" {
		name = "image"
	}
	slog.Debug("telegram: sending image", "chat_id", rc.chatID, "name", name, "size", len(img.Data))
	params := &tgbot.SendPhotoParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
		Photo:           &models.InputFileUpload{Filename: name, Data: bytes.NewReader(img.Data)},
		ReplyParameters: replyParameters(rc.messageID),
	}
	if _, err := bot.SendPhoto(ctx, params); err != nil {
		return fmt.Errorf("telegram: send image: %w", err)
	}
	return nil
}

func (p *Platform) SendFile(ctx context.Context, rctx any, file core.FileAttachment) error {
	rc, err := resolveReplyContext(ctx, rctx)
	if err != nil {
		return err
	}
	bot, err := p.connectedBot("send file")
	if err != nil {
		return err
	}

	name := file.FileName
	if name == "" {
		name = "attachment"
	}
	params := &tgbot.SendDocumentParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
		Document:        &models.InputFileUpload{Filename: name, Data: bytes.NewReader(file.Data)},
		ReplyParameters: replyParameters(rc.messageID),
	}
	if _, err := bot.SendDocument(ctx, params); err != nil {
		return fmt.Errorf("telegram: send file: %w", err)
	}
	return nil
}

// SendAudio sends synthesized audio back to Telegram.
// It prefers voice messages and falls back to audio files for mp3/m4a on sendVoice failure.
func (p *Platform) SendAudio(ctx context.Context, rctx any, audio []byte, format string) error {
	rc, err := resolveReplyContext(ctx, rctx)
	if err != nil {
		return err
	}

	sendData := audio
	sendFormat := strings.ToLower(strings.TrimSpace(format))
	if sendFormat == "" {
		sendFormat = "ogg"
	}

	switch sendFormat {
	case "ogg", "opus", "mp3", "m4a":
		// Attempt these formats directly with sendVoice first.
	default:
		converted, err := telegramConvertAudioToOpus(ctx, audio, sendFormat)
		if err != nil {
			return fmt.Errorf("telegram: SendAudio: convert %s to opus: %w", sendFormat, err)
		}
		sendData = converted
		sendFormat = "opus"
	}

	if err := p.sendVoice(ctx, rc, sendData, sendFormat); err != nil {
		if sendFormat == "mp3" || sendFormat == "m4a" {
			if fallbackErr := p.sendAudio(ctx, rc, sendData, sendFormat); fallbackErr == nil {
				return nil
			} else {
				return fmt.Errorf(
					"telegram: SendAudio: %w",
					errors.Join(
						fmt.Errorf("sendVoice failed: %w", err),
						fmt.Errorf("sendAudio fallback failed: %w", fallbackErr),
					),
				)
			}
		}
		return fmt.Errorf("telegram: SendAudio: sendVoice: %w", err)
	}
	return nil
}

func (p *Platform) sendVoice(ctx context.Context, rc replyContext, audio []byte, format string) error {
	bot, err := p.connectedBot("send voice")
	if err != nil {
		return err
	}
	params := &tgbot.SendVoiceParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
		Voice:           &models.InputFileUpload{Filename: "tts_audio." + telegramAudioFileExt(format), Data: bytes.NewReader(audio)},
		ReplyParameters: replyParameters(rc.messageID),
	}
	if _, err := bot.SendVoice(ctx, params); err != nil {
		return err
	}
	return nil
}

func (p *Platform) sendAudio(ctx context.Context, rc replyContext, audio []byte, format string) error {
	bot, err := p.connectedBot("send audio")
	if err != nil {
		return err
	}
	params := &tgbot.SendAudioParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
		Audio:           &models.InputFileUpload{Filename: "tts_audio." + telegramAudioFileExt(format), Data: bytes.NewReader(audio)},
		ReplyParameters: replyParameters(rc.messageID),
	}
	if _, err := bot.SendAudio(ctx, params); err != nil {
		return err
	}
	return nil
}

func telegramAudioFileExt(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "oga":
		return "ogg"
	case "":
		return "bin"
	default:
		return strings.ToLower(strings.TrimSpace(format))
	}
}

// SendWithButtons sends a message with an inline keyboard.
func (p *Platform) SendWithButtons(ctx context.Context, rctx any, content string, buttons [][]core.ButtonOption) error {
	rc, err := resolveReplyContext(ctx, rctx)
	if err != nil {
		return err
	}
	bot, err := p.connectedBot("send with buttons")
	if err != nil {
		return err
	}

	var rows [][]models.InlineKeyboardButton
	for _, row := range buttons {
		var btns []models.InlineKeyboardButton
		for _, b := range row {
			btns = append(btns, models.InlineKeyboardButton{Text: b.Text, CallbackData: b.Data})
		}
		rows = append(rows, btns)
	}

	return p.sendChunked(ctx, bot, content, chunkSendOptions{
		chatID:       rc.chatID,
		threadID:     rc.threadID,
		replyTo:      replyParameters(rc.messageID),
		replyMarkup:  &models.InlineKeyboardMarkup{InlineKeyboard: rows},
		logMethod:    "SendWithButtons",
		logChunkInfo: true,
	})
}

// DeletePreviewMessage deletes a stale preview message so the caller can send a fresh one.
func (p *Platform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	h, ok := previewHandle.(*telegramPreviewHandle)
	if !ok {
		return fmt.Errorf("telegram: invalid preview handle type %T", previewHandle)
	}
	bot, err := p.connectedBot("delete preview")
	if err != nil {
		return err
	}
	_, err = bot.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: h.chatID, MessageID: h.messageID})
	if err != nil {
		slog.Debug("telegram: delete preview message failed", "error", err)
	}
	return err
}

func (p *Platform) downloadFile(parent context.Context, fileID string) ([]byte, error) {
	bot, err := p.connectedBot("download file")
	if err != nil {
		return nil, err
	}
	ctx, cancel := p.operationContext(parent, telegramAPITimeout)
	defer cancel()
	f, err := bot.GetFile(ctx, &tgbot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	if f.FilePath == "" {
		return nil, fmt.Errorf("get file: empty file_path returned for file_id %s", fileID)
	}
	link := bot.FileDownloadLink(f)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, fmt.Errorf("download file %s: build request: %w", fileID, err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file %s: %w", fileID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file %s: status %d", fileID, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// Formats:
	//   telegram:{chatID}                      - shared session, no topic
	//   telegram:{chatID}:{threadID}           - shared session, with topic
	//   telegram:{chatID}:{userID}             - per-user session, no topic
	//   telegram:{chatID}:{threadID}:{userID}  - per-user session, with topic
	parts := strings.SplitN(sessionKey, ":", 5)
	if len(parts) < 2 || parts[0] != "telegram" {
		return nil, fmt.Errorf("telegram: invalid session key %q", sessionKey)
	}
	chatID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("telegram: invalid chat ID in %q", sessionKey)
	}

	threadID := 0
	switch len(parts) {
	case 2:
		// telegram:{chatID}
	case 3:
		if p.shareSessionInChannel {
			// telegram:{chatID}:{threadID}
			threadID, _ = strconv.Atoi(parts[2])
		}
		// else: telegram:{chatID}:{userID} — no threadID
	case 4:
		// telegram:{chatID}:{threadID}:{userID}
		threadID, _ = strconv.Atoi(parts[2])
	}

	return replyContext{chatID: chatID, threadID: threadID}, nil
}

// ReconstructMessageReplyCtx restores a durable queued message's destination
// and source reference after a process restart.
func (p *Platform) ReconstructMessageReplyCtx(sessionKey, messageID string) (any, error) {
	rctx, err := p.ReconstructReplyCtx(sessionKey)
	if err != nil {
		return nil, err
	}
	id, err := strconv.Atoi(messageID)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("telegram: invalid message ID %q", messageID)
	}
	rc := rctx.(replyContext)
	rc.messageID = id
	return rc, nil
}

// telegramPreviewHandle stores the chat, thread, and message IDs for an editable preview message.
type telegramPreviewHandle struct {
	chatID    int64
	threadID  int
	messageID int
}

func (p *Platform) progressIntervalLocked() time.Duration {
	if p.progressInterval > 0 {
		return p.progressInterval
	}
	return telegramProgressInterval
}

// AcquireProgressUpdate serializes Telegram progress calls and waits for the
// shared spacing/backoff deadline. The caller must release after its API call.
func (p *Platform) AcquireProgressUpdate(ctx context.Context) (func(bool), error) {
	p.progressMu.Lock()
	if p.progressGate == nil {
		p.progressGate = make(chan struct{}, 1)
		p.progressGate <- struct{}{}
	}
	gate := p.progressGate
	p.progressMu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
	}

	var releaseOnce sync.Once
	release := func(attempted bool) {
		releaseOnce.Do(func() {
			p.progressMu.Lock()
			if attempted {
				next := time.Now().Add(p.progressIntervalLocked())
				if next.After(p.progressNext) {
					p.progressNext = next
				}
			}
			gate <- struct{}{}
			p.progressMu.Unlock()
		})
	}

	for {
		p.progressMu.Lock()
		delay := time.Until(p.progressNext)
		p.progressMu.Unlock()
		if delay <= 0 {
			return release, nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			release(false)
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// DeferProgressUpdates applies Telegram's global retry_after window to all
// concurrent staging writers on this platform instance.
func (p *Platform) DeferProgressUpdates(delay time.Duration) {
	if delay < 0 {
		return
	}
	p.progressMu.Lock()
	deferredUntil := time.Now().Add(delay)
	if deferredUntil.After(p.progressNext) {
		p.progressNext = deferredUntil
	}
	p.progressMu.Unlock()
}

func (p *Platform) ProgressUpdateRetryAfter(err error) (time.Duration, bool) {
	var rateErr *tgbot.TooManyRequestsError
	if !errors.As(err, &rateErr) {
		return 0, false
	}
	return time.Duration(max(0, rateErr.RetryAfter)) * time.Second, true
}

var _ core.ProgressUpdateScheduler = (*Platform)(nil)
var _ core.ProgressUpdateRetryClassifier = (*Platform)(nil)
var _ core.StagingProgressStyleProvider = (*Platform)(nil)

// StagingProgressStyle enables Telegram-safe rich staging presentation.
func (p *Platform) StagingProgressStyle() core.StagingProgressStyle {
	return core.StagingProgressStyle{
		ToolBodiesAsCode:  true,
		RepeatLiveHeader:  true,
		CompactOnComplete: true,
	}
}

// SendPreviewStart sends a new message and returns a handle for subsequent edits.
func (p *Platform) SendPreviewStart(ctx context.Context, rctx any, content string) (any, error) {
	rc, err := resolveReplyContext(ctx, rctx)
	if err != nil {
		return nil, err
	}
	bot, err := p.connectedBot("send preview")
	if err != nil {
		return nil, err
	}

	html := core.MarkdownToSimpleHTML(content)
	params := &tgbot.SendMessageParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
		Text:            html,
		ParseMode:       models.ParseModeHTML,
		ReplyParameters: replyParameters(rc.messageID),
	}

	sent, err := bot.SendMessage(ctx, params)
	if err != nil {
		if strings.Contains(err.Error(), "can't parse") {
			slog.Warn("telegram: HTML rejected by Telegram, sending preview as plain text",
				"method", "SendPreviewStart",
				"error", err.Error(),
				"html_prefix", truncateForLog(html, 200),
				"html_len", len(html),
			)
			params.Text = content
			params.ParseMode = ""
			sent, err = bot.SendMessage(ctx, params)
		}
		if err != nil {
			return nil, fmt.Errorf("telegram: send preview: %w", err)
		}
	}
	return &telegramPreviewHandle{chatID: rc.chatID, threadID: rc.threadID, messageID: sent.ID}, nil
}

// UpdateMessage edits an existing message identified by previewHandle.
func (p *Platform) UpdateMessage(ctx context.Context, previewHandle any, content string) error {
	h, ok := previewHandle.(*telegramPreviewHandle)
	if !ok {
		return fmt.Errorf("telegram: invalid preview handle type %T", previewHandle)
	}
	bot, err := p.connectedBot("update message")
	if err != nil {
		return err
	}

	html := core.MarkdownToSimpleHTML(content)
	slog.Debug("telegram: UpdateMessage",
		"content_len", len(content), "html_len", len(html),
		"content_prefix", truncateForLog(content, 80),
		"html_prefix", truncateForLog(html, 80))

	params := &tgbot.EditMessageTextParams{
		ChatID:    h.chatID,
		MessageID: h.messageID,
		Text:      html,
		ParseMode: models.ParseModeHTML,
	}

	if _, err := bot.EditMessageText(ctx, params); err != nil {
		errMsg := err.Error()
		slog.Debug("telegram: UpdateMessage HTML failed", "error", errMsg)
		if strings.Contains(errMsg, "not modified") {
			return nil
		}
		if strings.Contains(errMsg, "can't parse") {
			slog.Warn("telegram: HTML rejected by Telegram, editing as plain text",
				"method", "UpdateMessage",
				"error", errMsg,
				"html_prefix", truncateForLog(html, 200),
				"html_len", len(html),
			)
			params.Text = content
			params.ParseMode = ""
			if _, err2 := bot.EditMessageText(ctx, params); err2 != nil {
				if strings.Contains(err2.Error(), "not modified") {
					return nil
				}
				return fmt.Errorf("telegram: edit message: %w", err2)
			}
			return nil
		}
		return fmt.Errorf("telegram: edit message: %w", err)
	}
	slog.Debug("telegram: UpdateMessage HTML success")
	return nil
}

// StartTyping sends a "typing…" chat action and repeats every 5 seconds
// until the returned stop function is called.
func (p *Platform) StartTyping(ctx context.Context, rctx any) (stop func()) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return func() {}
	}

	params := &tgbot.SendChatActionParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
		Action:          models.ChatActionTyping,
	}

	if bot, err := p.connectedBot("typing"); err == nil {
		if _, err := bot.SendChatAction(ctx, params); err != nil {
			slog.Debug("telegram: initial typing send failed", "error", err)
		}
	} else {
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		ticker := p.newTypingTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C():
				bot, err := p.connectedBot("typing")
				if err != nil {
					slog.Debug("telegram: typing stopped", "error", err)
					return
				}
				if _, err := bot.SendChatAction(ctx, params); err != nil {
					slog.Debug("telegram: typing send failed", "error", err)
				}
			}
		}
	}()

	return func() { close(done) }
}

func truncateForLog(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "..."
}

const telegramBotCommandDescriptionLimit = 40

// truncateTelegramBotDescription keeps Telegram command descriptions within a
// conservative safety budget. Telegram documents a larger per-field limit, but
// shorter descriptions avoid command menu registration failures when many
// commands are installed. Byte slicing breaks UTF-8 for CJK text and triggers
// "text must be encoded in UTF-8" from the API (#119).
func truncateTelegramBotDescription(s string) string {
	const max = telegramBotCommandDescriptionLimit
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-3]) + "..."
}

func (p *Platform) Stop() error {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return nil
	}
	p.stopping = true
	cancel := p.cancel
	p.cancel = nil
	p.bot = nil
	p.selfUser = nil
	p.mu.Unlock()

	p.cancelMediaGroups()
	p.generalRouteMu.Lock()
	clear(p.generalRoutes)
	p.generalRouteMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// RegisterCommands registers bot commands with Telegram for the command menu.
func (p *Platform) RegisterCommands(commands []core.BotCommandInfo) error {
	bot, err := p.connectedBot("register commands")
	if err != nil {
		return err
	}

	// Telegram limits: max 100 commands; keep descriptions conservatively short
	// to avoid menu registration failures with larger command sets.
	var tgCommands []models.BotCommand
	seen := make(map[string]bool)
	for _, c := range commands {
		cmd := sanitizeTelegramCommand(c.Command)
		if cmd == "" || seen[cmd] {
			continue
		}
		seen[cmd] = true
		desc := truncateTelegramBotDescription(c.Description)
		tgCommands = append(tgCommands, models.BotCommand{
			Command:     cmd,
			Description: desc,
		})
	}

	// Limit to 100 commands
	if len(tgCommands) > 100 {
		tgCommands = tgCommands[:100]
	}

	if len(tgCommands) == 0 {
		slog.Debug("telegram: no commands to register")
		return nil
	}

	ctx := context.Background()
	if _, err := bot.SetMyCommands(ctx, &tgbot.SetMyCommandsParams{Commands: tgCommands}); err != nil {
		return fmt.Errorf("telegram: setMyCommands failed: %w", err)
	}

	slog.Info("telegram: registered bot commands", "count", len(tgCommands))
	return nil
}

// extractEntityText extracts a substring from text using Telegram's UTF-16 code unit
// offset and length. Telegram Bot API entity offsets are measured in UTF-16 code units,
// not bytes or Unicode code points, so direct byte slicing produces wrong results
// when the text contains non-ASCII characters (e.g. Chinese, emoji).
func extractEntityText(text string, offsetUTF16, lengthUTF16 int) string {
	encoded := utf16.Encode([]rune(text))
	endUTF16 := offsetUTF16 + lengthUTF16
	if offsetUTF16 < 0 || lengthUTF16 < 0 || endUTF16 > len(encoded) {
		return ""
	}
	return string(utf16.Decode(encoded[offsetUTF16:endUTF16]))
}

// sanitizeTelegramCommand converts a command name to Telegram-compatible format.
// Telegram rules: 1-32 chars, lowercase letters/digits/underscores, must start with a letter.
// Returns "" if the command cannot be sanitized (e.g. empty or no letter to start with).
func sanitizeTelegramCommand(cmd string) string {
	cmd = strings.ToLower(cmd)
	var b strings.Builder
	for _, c := range cmd {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	result := b.String()
	// Collapse consecutive underscores
	for strings.Contains(result, "__") {
		result = strings.ReplaceAll(result, "__", "_")
	}
	result = strings.Trim(result, "_")
	// Must start with a letter
	if len(result) == 0 || result[0] < 'a' || result[0] > 'z' {
		return ""
	}
	if len(result) > 32 {
		result = result[:32]
	}
	return result
}

var _ core.AudioSender = (*Platform)(nil)
