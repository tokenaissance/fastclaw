package channels

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// Telegram implements the Channel interface for Telegram Bot API.
type Telegram struct {
	bot *tgbotapi.BotAPI
	bus *bus.MessageBus
}

// NewTelegram creates a new Telegram channel.
func NewTelegram(botToken string, mb *bus.MessageBus) (*Telegram, error) {
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	slog.Info("telegram bot authorized", "username", bot.Self.UserName)

	return &Telegram{
		bot: bot,
		bus: mb,
	}, nil
}

func (t *Telegram) Name() string {
	return "telegram"
}

// Start begins long polling for Telegram updates.
func (t *Telegram) Start(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := t.bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			t.bot.StopReceivingUpdates()
			return nil
		case update := <-updates:
			if update.Message == nil || update.Message.Text == "" {
				continue
			}

			slog.Info("telegram message received",
				"from", update.Message.From.UserName,
				"chat_id", update.Message.Chat.ID,
				"text", update.Message.Text,
			)

			t.bus.Inbound <- bus.InboundMessage{
				Channel: "telegram",
				ChatID:  strconv.FormatInt(update.Message.Chat.ID, 10),
				UserID:  strconv.FormatInt(update.Message.From.ID, 10),
				Text:    update.Message.Text,
			}
		}
	}
}

// Send sends a text message to a Telegram chat.
func (t *Telegram) Send(chatID string, text string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse chat ID: %w", err)
	}

	msg := tgbotapi.NewMessage(id, text)
	_, err = t.bot.Send(msg)
	return err
}
