package gateway

import (
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// registerChannels creates channel instances from config, one per account.
func registerChannels(cfg *config.Config, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	for name, chCfg := range cfg.Channels {
		if !chCfg.Enabled {
			continue
		}

		switch name {
		case "telegram":
			if err := registerTelegramChannels(chCfg, mb, chanMgr); err != nil {
				return err
			}
		case "discord":
			if err := registerDiscordChannels(chCfg, mb, chanMgr); err != nil {
				return err
			}
		case "slack":
			if err := registerSlackChannels(chCfg, mb, chanMgr); err != nil {
				return err
			}
		}
	}
	return nil
}

func registerTelegramChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	if len(chCfg.Accounts) == 0 {
		// No accounts defined — use the channel-level botToken as the default account
		tg, err := channels.NewTelegram(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		chanMgr.Register(tg)
		return nil
	}

	// One instance per account
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken // fall back to parent
		}
		tg, err := channels.NewTelegram(token, accountID, mb)
		if err != nil {
			return err
		}
		chanMgr.Register(tg)
	}
	return nil
}

func registerDiscordChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	if len(chCfg.Accounts) == 0 {
		dc, err := channels.NewDiscord(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		chanMgr.Register(dc)
		return nil
	}

	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		dc, err := channels.NewDiscord(token, accountID, mb)
		if err != nil {
			return err
		}
		chanMgr.Register(dc)
	}
	return nil
}

func registerSlackChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	if len(chCfg.Accounts) == 0 {
		sl, err := channels.NewSlack(chCfg.BotToken, chCfg.AppToken, "", mb)
		if err != nil {
			return err
		}
		chanMgr.Register(sl)
		return nil
	}

	for accountID, acct := range chCfg.Accounts {
		botToken := acct.BotToken
		if botToken == "" {
			botToken = chCfg.BotToken
		}
		sl, err := channels.NewSlack(botToken, chCfg.AppToken, accountID, mb)
		if err != nil {
			return err
		}
		chanMgr.Register(sl)
	}
	return nil
}

// buildBotUsernames creates agentID -> botUsername mapping by looking at bindings
// and resolving the bot username from the channel manager.
func buildBotUsernames(bindings []config.Binding, chanMgr *channels.Manager) map[string]string {
	m := make(map[string]string)
	for _, b := range bindings {
		if b.Match.Channel == "" {
			continue
		}
		username := chanMgr.BotUsername(b.Match.Channel, b.Match.AccountID)
		if username != "" {
			m[b.AgentID] = username
		}
	}
	return m
}
