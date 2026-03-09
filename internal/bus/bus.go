package bus

// InboundMessage represents a message received from a channel.
type InboundMessage struct {
	Channel string // channel type, e.g. "telegram"
	ChatID  string // unique chat identifier within the channel
	UserID  string // user identifier
	Text    string // message text
}

// OutboundMessage represents a message to be sent to a channel.
type OutboundMessage struct {
	Channel string // target channel type
	ChatID  string // target chat identifier
	Text    string // message text
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
