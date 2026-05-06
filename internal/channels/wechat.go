package channels

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// WeChat implements the Channel interface for the iLink (微信) bot
// platform. Pattern mirrors telegram.go: a single file owning the
// HTTP client, long-poll loop, and outbound send. We deliberately
// don't import a higher-level library — keeping the protocol surface
// in-tree makes it easy to evolve alongside fastclaw's own message
// types and avoids a Go-module dependency on an out-of-tree project.

// iLink protocol constants. Matches the upstream WeChat bot API.
const (
	wechatDefaultBaseURL    = "https://ilinkai.weixin.qq.com"
	wechatLongPollTimeout   = 35 * time.Second
	wechatSendTimeout       = 15 * time.Second
	wechatErrSessionExpired = -14 // server-side sync token rotated; reset and re-poll

	wechatMsgTypeUser = 1
	wechatMsgTypeBot  = 2

	wechatMsgStateFinish = 2

	wechatItemTypeText  = 1
	wechatItemTypeImage = 2
	wechatItemTypeVoice = 3
	wechatItemTypeFile  = 4
	wechatItemTypeVideo = 5

	wechatBackoffInitial = 3 * time.Second
	wechatBackoffMax     = 60 * time.Second

	// /ilink/bot/sendtyping status values.
	wechatTypingStatusTyping = 1
	wechatTypingStatusCancel = 2

	wechatTypingTimeout = 8 * time.Second
)

// WeChat is the iLink long-poll adapter for one logged-in WeChat bot.
type WeChat struct {
	bus       *bus.MessageBus
	accountID string // ilink_bot_id, used by routing to look up the owner

	// HTTP credentials (one-time on QR confirm, persisted in configs):
	botToken    string
	baseURL     string
	ilinkUserID string

	httpClient *http.Client
	wechatUIN  string // randomized per process; iLink wants a stable-ish header

	// Long-poll cursor. iLink's `get_updates_buf` advances each turn;
	// keeping it on the struct (vs. on disk like the upstream daemon
	// does) is fine because fastclaw's gateway holds one *WeChat per
	// (account, process lifetime). Process restart re-syncs from "" —
	// iLink replays from the last-seen sequence on the server side
	// rather than re-delivering, so duplicates are handled there.
	getUpdatesBuf string
	failures      int

	// Per-chat ContextToken cache. The /ilink/bot/getconfig call that
	// mints typing_ticket wants the latest context_token from the user's
	// inbound message; we don't get one in SendTyping(chatID) so we
	// remember the most recent token off each inbound and use it on the
	// way back. Empty string is allowed (getconfig has it as optional)
	// — the cache is best-effort, not a hard prerequisite.
	ctxTokensMu sync.Mutex
	ctxTokens   map[string]string

	// onExpired fires once when the iLink server has confirmed the bot
	// token is dead (operator must rescan). Set by the gateway so it
	// can disable the configs row + unregister the adapter; without it
	// the loop would log the same warning every 5s forever.
	onExpired func(accountID string)
}

// SetOnExpired registers a callback that fires when the bot token is
// confirmed dead. The callback runs once; Start exits afterwards.
func (w *WeChat) SetOnExpired(fn func(accountID string)) {
	w.onExpired = fn
}

// NewWeChat creates a new WeChat channel adapter from a connected
// account's stored credentials.
func NewWeChat(botToken, baseURL, ilinkUserID, accountID string, mb *bus.MessageBus) (*WeChat, error) {
	if botToken == "" || accountID == "" {
		return nil, fmt.Errorf("wechat: botToken and accountID required")
	}
	if baseURL == "" {
		baseURL = wechatDefaultBaseURL
	}
	slog.Info("wechat bot authorized", "account", accountID)
	return &WeChat{
		bus:         mb,
		accountID:   accountID,
		botToken:    botToken,
		baseURL:     baseURL,
		ilinkUserID: ilinkUserID,
		httpClient:  &http.Client{},
		wechatUIN:   wechatGenerateUIN(),
		ctxTokens:   make(map[string]string),
	}, nil
}

func (w *WeChat) Name() string        { return "wechat" }
func (w *WeChat) AccountID() string   { return w.accountID }
func (w *WeChat) BotUsername() string { return w.accountID }

// Start runs the long-poll loop until ctx is cancelled. Mirrors the
// retry / session-recovery semantics of the upstream weclaw monitor:
//   - any GetUpdates error → exponential backoff up to 60s
//   - errcode -14 (session expired) → reset sync buf and retry; if
//     the sync buf was already empty the bot token itself is dead
//     (operator needs to re-scan).
func (w *WeChat) Start(ctx context.Context) error {
	slog.Info("wechat long-poll loop starting", "account", w.accountID)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		resp, err := w.getUpdates(ctx, w.getUpdatesBuf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.failures++
			backoff := w.calcBackoff()
			slog.Warn("wechat getUpdates error",
				"account", w.accountID, "failures", w.failures, "backoff", backoff, "error", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		w.failures = 0

		if resp.ErrCode == wechatErrSessionExpired {
			if w.getUpdatesBuf != "" {
				slog.Info("wechat session expired, resetting sync buf", "account", w.accountID)
				w.getUpdatesBuf = ""
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
					return nil
				}
				continue
			}
			// Sync buf was already empty: server is telling us the bot
			// token itself is dead. Continuing to poll just spams the
			// same warning every 5s forever — instead, log once, fire
			// the registered onExpired callback (the gateway disables
			// the configs row + unregisters us), and exit.
			slog.Warn("wechat bot token expired — user must rescan QR", "account", w.accountID)
			if w.onExpired != nil {
				w.onExpired(w.accountID)
			}
			return nil
		}
		if resp.Ret != 0 && resp.ErrCode != 0 {
			slog.Warn("wechat server error",
				"account", w.accountID, "ret", resp.Ret, "errcode", resp.ErrCode, "errmsg", resp.ErrMsg)
			continue
		}
		if resp.GetUpdatesBuf != "" {
			w.getUpdatesBuf = resp.GetUpdatesBuf
		}
		for _, m := range resp.Msgs {
			w.dispatchInbound(m)
		}
	}
}

// dispatchInbound flattens a iLink message into a bus.InboundMessage.
// Filter rules:
//   - drop messages from the bot itself (MessageType=2). They're echoes
//     of our own sends.
//   - drop in-progress streaming messages (MessageState != finish);
//     iLink sends partial deltas during voice transcription, we only
//     want the final.
//   - text + image are surfaced; voice is surfaced as the
//     speech-to-text transcription iLink already provides; video / file
//     are dropped (we don't have download/decrypt support yet — adding
//     it requires AES-128-ECB CDN handling, deferred).
func (w *WeChat) dispatchInbound(m wechatMessage) {
	if m.MessageType != wechatMsgTypeUser {
		return
	}
	if m.MessageState != wechatMsgStateFinish {
		return
	}

	var text string
	for _, item := range m.ItemList {
		switch item.Type {
		case wechatItemTypeText:
			if item.TextItem != nil && item.TextItem.Text != "" {
				text = item.TextItem.Text
			}
		case wechatItemTypeVoice:
			// iLink ships speech-to-text transcription alongside the
			// audio bytes — use it directly so the agent sees the
			// user's spoken request as text without us having to
			// download + transcribe ourselves.
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				text = item.VoiceItem.Text
			}
		}
		if text != "" {
			break
		}
	}
	if text == "" {
		slog.Debug("wechat skipping unsupported message",
			"account", w.accountID, "from", m.FromUserID, "items", len(m.ItemList))
		return
	}

	// iLink doesn't distinguish DM vs group at the protocol level the
	// way Telegram does — every message has a from_user_id and a
	// to_user_id (the bot). Treat all as DM for now; group support
	// would require parsing room_id which the current iLink response
	// shape doesn't expose.
	slog.Info("wechat message received",
		"account", w.accountID, "from", m.FromUserID, "len", len(text))

	// Remember this user's most recent ContextToken so a subsequent
	// SendTyping(chatID) can mint a typing_ticket without round-trip-
	// owning the original message. Cache is per-chat; we just overwrite
	// — the freshest token is the most likely to validate.
	if m.FromUserID != "" {
		w.ctxTokensMu.Lock()
		w.ctxTokens[m.FromUserID] = m.ContextToken
		w.ctxTokensMu.Unlock()
	}

	w.bus.Inbound <- bus.InboundMessage{
		Channel:   "wechat",
		AccountID: w.accountID,
		ChatID:    m.FromUserID, // 1:1 — sender is also the chat key
		UserID:    m.FromUserID,
		MessageID: strconv.FormatInt(m.MessageID, 10),
		Text:      text,
		PeerKind:  "dm",
	}
}

// Send sends a plain text message — the simple form. Used by tools
// that don't need rich formatting.
func (w *WeChat) Send(chatID, text string) error {
	return w.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage posts a reply to a iLink user. iLink doesn't have native
// markdown, inline keyboards, or message edits, so most of the
// OutboundMessage fields are intentionally ignored — we only honor
// Text, ChatID, and ContextToken (carried implicitly via ReplyToMsgID
// on iLink's side; see context_token note below).
func (w *WeChat) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	// iLink wants markdown stripped — clients render plain text and
	// will literally show *bold* / [link](url) syntax. Strip it
	// best-effort, same way weclaw's MarkdownToPlainText helper does.
	plain := wechatStripMarkdown(msg.Text)
	if plain == "" {
		// No usable text to send (image-only output not supported yet
		// — see dispatchInbound's TODO). Log and drop rather than
		// posting an empty payload that iLink would reject.
		slog.Debug("wechat send: nothing to send after markdown strip", "chat", msg.ChatID)
		return nil
	}
	body := wechatSendRequest{
		Msg: wechatSendMsg{
			FromUserID:   w.accountID,
			ToUserID:     msg.ChatID,
			ClientID:     uuid.NewString(),
			MessageType:  wechatMsgTypeBot,
			MessageState: wechatMsgStateFinish,
			ItemList: []wechatItem{
				{
					Type:     wechatItemTypeText,
					TextItem: &wechatTextItem{Text: plain},
				},
			},
		},
		BaseInfo: wechatBaseInfo{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), wechatSendTimeout)
	defer cancel()
	var resp wechatSendResponse
	if err := w.doPost(ctx, "/ilink/bot/sendmessage", body, &resp); err != nil {
		return fmt.Errorf("wechat send: %w", err)
	}
	if resp.Ret != 0 {
		return fmt.Errorf("wechat send: ret=%d errmsg=%s", resp.Ret, resp.ErrMsg)
	}
	return nil
}

// SendTyping shows a "对方正在输入..." indicator on the user's WeChat
// while the agent is processing the turn. iLink wants two calls:
//
//  1. /ilink/bot/getconfig with the recipient's ilink_user_id (and
//     optionally their last context_token) to mint a typing_ticket;
//  2. /ilink/bot/sendtyping with that ticket and status=1.
//
// The gateway pings this every 5s for the duration of a turn — same
// cadence as Telegram's sendChatAction. Errors are logged at Debug and
// returned, but the gateway treats them as best-effort, so a hiccup
// doesn't fail the user-visible reply.
func (w *WeChat) SendTyping(chatID string) error {
	if chatID == "" {
		return nil
	}
	w.ctxTokensMu.Lock()
	contextToken := w.ctxTokens[chatID]
	w.ctxTokensMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), wechatTypingTimeout)
	defer cancel()

	cfgBody := wechatGetConfigRequest{
		ILinkUserID:  chatID,
		ContextToken: contextToken,
	}
	var cfgResp wechatGetConfigResponse
	if err := w.doPost(ctx, "/ilink/bot/getconfig", cfgBody, &cfgResp); err != nil {
		slog.Warn("wechat getconfig failed", "account", w.accountID, "chat", chatID, "error", err)
		return fmt.Errorf("wechat getconfig: %w", err)
	}
	if cfgResp.Ret != 0 {
		slog.Warn("wechat getconfig non-zero ret",
			"account", w.accountID, "chat", chatID, "ret", cfgResp.Ret, "errmsg", cfgResp.ErrMsg)
		return fmt.Errorf("wechat getconfig: ret=%d errmsg=%s", cfgResp.Ret, cfgResp.ErrMsg)
	}
	if cfgResp.TypingTicket == "" {
		slog.Info("wechat getconfig returned empty typing_ticket — typing disabled for this account",
			"account", w.accountID, "chat", chatID)
		return nil
	}

	typingBody := wechatSendTypingRequest{
		ILinkUserID:  chatID,
		TypingTicket: cfgResp.TypingTicket,
		Status:       wechatTypingStatusTyping,
	}
	var typingResp wechatSendTypingResponse
	if err := w.doPost(ctx, "/ilink/bot/sendtyping", typingBody, &typingResp); err != nil {
		slog.Warn("wechat sendtyping failed", "account", w.accountID, "chat", chatID, "error", err)
		return fmt.Errorf("wechat sendtyping: %w", err)
	}
	if typingResp.Ret != 0 {
		slog.Warn("wechat sendtyping non-zero ret",
			"account", w.accountID, "chat", chatID, "ret", typingResp.Ret, "errmsg", typingResp.ErrMsg)
		return fmt.Errorf("wechat sendtyping: ret=%d errmsg=%s", typingResp.Ret, typingResp.ErrMsg)
	}
	slog.Debug("wechat typing sent", "account", w.accountID, "chat", chatID)
	return nil
}

// --- HTTP plumbing ---

// getUpdates is the long-poll. Server holds the request open up to
// `longpolling_timeout_ms` (typically 30s) and returns either pending
// messages or an empty Msgs slice. We give the request 5s of slack
// over the server-side timeout so client-side cancellation is
// distinguishable from server-side empty-batch.
func (w *WeChat) getUpdates(ctx context.Context, buf string) (*wechatGetUpdatesResponse, error) {
	body := wechatGetUpdatesRequest{
		GetUpdatesBuf: buf,
		BaseInfo:      wechatBaseInfo{ChannelVersion: "1.0.0"},
	}
	ctx, cancel := context.WithTimeout(ctx, wechatLongPollTimeout+5*time.Second)
	defer cancel()
	var resp wechatGetUpdatesResponse
	if err := w.doPost(ctx, "/ilink/bot/getupdates", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (w *WeChat) doPost(ctx context.Context, path string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("Authorization", "Bearer "+w.botToken)
	req.Header.Set("X-WECHAT-UIN", w.wechatUIN)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return json.Unmarshal(respBody, result)
}

func (w *WeChat) calcBackoff() time.Duration {
	d := wechatBackoffInitial
	for i := 1; i < w.failures; i++ {
		d *= 2
		if d > wechatBackoffMax {
			return wechatBackoffMax
		}
	}
	return d
}

// wechatGenerateUIN produces the randomized base64 string iLink wants
// in the X-WECHAT-UIN header. The upstream protocol documents it as
// "anything stable-ish per process"; we generate once at adapter
// construction.
func wechatGenerateUIN() string {
	var n uint32
	_ = binary.Read(rand.Reader, binary.LittleEndian, &n)
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", n)))
}

// wechatStripMarkdown is a small best-effort plain-text converter so
// LLM-emitted markdown doesn't show up as raw `*foo*` / `### bar` in
// WeChat. Not a full parser — we only handle the common offenders.
func wechatStripMarkdown(text string) string {
	if text == "" {
		return ""
	}
	out := text
	// Strip ATX headers at line starts
	for _, prefix := range []string{"### ", "## ", "# "} {
		out = bytesReplaceAtLineStart(out, prefix, "")
	}
	// Bold/italic markers — drop the markers themselves
	out = bytesReplaceAll(out, "**", "")
	out = bytesReplaceAll(out, "__", "")
	// Inline code backticks — drop
	out = bytesReplaceAll(out, "```", "")
	out = bytesReplaceAll(out, "`", "")
	return out
}

func bytesReplaceAll(s, old, new string) string {
	if old == "" {
		return s
	}
	for {
		i := indexOf(s, old)
		if i < 0 {
			return s
		}
		s = s[:i] + new + s[i+len(old):]
	}
}

func bytesReplaceAtLineStart(s, prefix, replacement string) string {
	if prefix == "" {
		return s
	}
	out := make([]byte, 0, len(s))
	atLineStart := true
	for i := 0; i < len(s); {
		if atLineStart && i+len(prefix) <= len(s) && s[i:i+len(prefix)] == prefix {
			out = append(out, replacement...)
			i += len(prefix)
			atLineStart = false
			continue
		}
		out = append(out, s[i])
		atLineStart = s[i] == '\n'
		i++
	}
	return string(out)
}

func indexOf(s, sub string) int {
	if sub == "" {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- Wire types (iLink protocol shape, kept private to this package) ---

type wechatBaseInfo struct {
	ChannelVersion string `json:"channel_version,omitempty"`
}

type wechatGetUpdatesRequest struct {
	GetUpdatesBuf string         `json:"get_updates_buf"`
	BaseInfo      wechatBaseInfo `json:"base_info"`
}

type wechatGetUpdatesResponse struct {
	Ret           int             `json:"ret"`
	ErrCode       int             `json:"errcode,omitempty"`
	ErrMsg        string          `json:"errmsg,omitempty"`
	Msgs          []wechatMessage `json:"msgs"`
	GetUpdatesBuf string          `json:"get_updates_buf"`
}

type wechatMessage struct {
	Seq          int          `json:"seq,omitempty"`
	MessageID    int64        `json:"message_id,omitempty"`
	FromUserID   string       `json:"from_user_id"`
	ToUserID     string       `json:"to_user_id"`
	MessageType  int          `json:"message_type"`
	MessageState int          `json:"message_state"`
	ItemList     []wechatItem `json:"item_list"`
	ContextToken string       `json:"context_token"`
}

type wechatItem struct {
	Type      int              `json:"type"`
	TextItem  *wechatTextItem  `json:"text_item,omitempty"`
	VoiceItem *wechatVoiceItem `json:"voice_item,omitempty"`
}

type wechatTextItem struct {
	Text string `json:"text"`
}

type wechatVoiceItem struct {
	Text     string `json:"text,omitempty"`     // STT transcription
	Playtime int    `json:"playtime,omitempty"` // ms
}

type wechatSendRequest struct {
	Msg      wechatSendMsg  `json:"msg"`
	BaseInfo wechatBaseInfo `json:"base_info"`
}

type wechatSendMsg struct {
	FromUserID   string       `json:"from_user_id"`
	ToUserID     string       `json:"to_user_id"`
	ClientID     string       `json:"client_id"`
	MessageType  int          `json:"message_type"`
	MessageState int          `json:"message_state"`
	ItemList     []wechatItem `json:"item_list"`
	ContextToken string       `json:"context_token,omitempty"`
}

type wechatSendResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}

type wechatGetConfigRequest struct {
	ILinkUserID  string         `json:"ilink_user_id"`
	ContextToken string         `json:"context_token,omitempty"`
	BaseInfo     wechatBaseInfo `json:"base_info"`
}

type wechatGetConfigResponse struct {
	Ret          int    `json:"ret"`
	ErrMsg       string `json:"errmsg,omitempty"`
	TypingTicket string `json:"typing_ticket,omitempty"`
}

type wechatSendTypingRequest struct {
	ILinkUserID  string         `json:"ilink_user_id"`
	TypingTicket string         `json:"typing_ticket"`
	Status       int            `json:"status"`
	BaseInfo     wechatBaseInfo `json:"base_info"`
}

type wechatSendTypingResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}
