package gateway

import (
	"encoding/json"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// registerChannelInstance starts a channel adapter for one kind="channel"
// row in configs. The row's credential_key is what processInbound
// reverse-looks up via Store.LookupChannelByCredential to find the owner —
// keep it stable (e.g. tail of bot token, app id).
func registerChannelInstance(rec store.ConfigRecord, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	cc := decodeChannelConfig(rec)
	switch rec.Name {
	case "telegram":
		return registerTelegramChannels(cc, mb, chanMgr)
	case "discord":
		return registerDiscordChannels(cc, mb, chanMgr)
	case "slack":
		return registerSlackChannels(cc, mb, chanMgr)
	}
	return nil
}

func decodeChannelConfig(rec store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	return cc
}

func registerTelegramChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	if len(chCfg.Accounts) == 0 {
		tg, err := channels.NewTelegram(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		chanMgr.Register(tg)
		return nil
	}
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
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

// buildBotUsernames creates agentID -> botUsername mapping by looking at
// bindings and resolving the bot username from the channel manager.
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
