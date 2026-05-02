package bus

// InboundMessage represents a message received from a channel.
type InboundMessage struct {
	Channel      string   // channel type, e.g. "telegram"
	AccountID    string   // account within the channel (e.g. which bot)
	ChatID       string   // unique chat identifier within the channel
	UserID       string   // user identifier
	OwnerUserID  string   // fastclaw user that owns the agent (for multi-user routing)
	MessageID    string   // unique message identifier within the chat
	Text         string   // message text
	PeerKind     string   // "group" or "dm"
	SenderName   string   // display name of the sender
	Mentions     []string // @usernames mentioned in the message
	IsBotMessage bool     // true if the message was sent by a bot
	PhotoURL     string   // URL of attached photo (if any) — single-image legacy field
	PhotoURLs    []string // URLs of attached photos. Independent of PhotoURL so old single-image callers (Telegram bridge etc.) keep working untouched; new web-chat path uses this for multi-image attachments.
	ReplyToMsgID string   // message ID being replied to
}

// OutboundButton represents a button in an inline keyboard.
type OutboundButton struct {
	Text         string
	CallbackData string
	URL          string
}

// MediaItem is an attachment whose bytes are already resolved (read
// from workspace.Store / sandbox snapshot / wherever) by the time the
// message lands on the bus. Channels that can't access the host
// filesystem (e2b path) still need to upload to Telegram/Discord/etc.,
// so we ship the bytes inline rather than asking each channel adapter
// to hold a workspace.Store reference.
type MediaItem struct {
	Filename    string // for content-type sniffing + display in IM
	ContentType string // optional override; channels can sniff if empty
	Bytes       []byte
}

// OutboundMessage represents a message to be sent to a channel.
type OutboundMessage struct {
	Channel      string             // target channel type
	AccountID    string             // target account within the channel
	AgentID      string             // originating agent — used by the WebChannel to route SSE events to the right (agent, session) pair; harmless for IM channels (which key on AccountID).
	ChatID       string             // target chat identifier
	Text         string             // message text
	ReplyToMsgID string             // reply to specific message
	ParseMode    string             // "MarkdownV2", "HTML", ""
	Buttons      [][]OutboundButton // inline keyboard rows
	EditMsgID    string             // edit existing message instead of sending new
	MediaPaths   []string           // file paths to attach (from MEDIA: protocol; host-mounted backends only)
	MediaItems   []MediaItem        // pre-resolved attachments — channel uploads bytes directly
}

// MessageBus is an async message queue backed by Go channels.
type MessageBus struct {
	Inbound  chan InboundMessage
	Outbound chan OutboundMessage
}

// New creates a new MessageBus with buffered channels.
func New() *MessageBus {
	return &MessageBus{
		Inbound:  make(chan InboundMessage, 100),
		Outbound: make(chan OutboundMessage, 100),
	}
}
