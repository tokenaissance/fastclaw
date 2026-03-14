package bus

// InboundMessage represents a message received from a channel.
type InboundMessage struct {
	Channel      string   // channel type, e.g. "telegram"
	AccountID    string   // account within the channel (e.g. which bot)
	ChatID       string   // unique chat identifier within the channel
	UserID       string   // user identifier
	MessageID    string   // unique message identifier within the chat
	Text         string   // message text
	PeerKind     string   // "group" or "dm"
	SenderName   string   // display name of the sender
	Mentions     []string // @usernames mentioned in the message
	IsBotMessage bool     // true if the message was sent by a bot
}

// OutboundMessage represents a message to be sent to a channel.
type OutboundMessage struct {
	Channel   string // target channel type
	AccountID string // target account within the channel
	ChatID    string // target chat identifier
	Text      string // message text
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
