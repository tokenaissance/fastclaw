package channels

import "context"

// Channel is the interface that all channel implementations must satisfy.
type Channel interface {
	// Name returns the channel identifier (e.g. "telegram").
	Name() string
	// Start begins listening for messages. It should block until ctx is cancelled.
	Start(ctx context.Context) error
	// Send sends a message to the specified chat.
	Send(chatID string, text string) error
}
