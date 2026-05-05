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
//
// `hot` controls whether the bot adapter's polling goroutine is launched
// immediately. Boot-time registration uses Register (Manager.Start
// fans out everything in one go); dashboard mutations use
// RegisterAndStart so a freshly-saved bot starts receiving updates
// without restarting the process.
func registerChannelInstance(rec store.ConfigRecord, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	cc := decodeChannelConfig(rec)
	switch rec.Name {
	case "telegram":
		return registerTelegramChannels(cc, mb, chanMgr, hot)
	case "discord":
		return registerDiscordChannels(cc, mb, chanMgr, hot)
	case "slack":
		return registerSlackChannels(cc, mb, chanMgr, hot)
	case "line":
		return registerLINEChannels(cc, mb, chanMgr, hot)
	case "wechat":
		return registerWeChatChannels(cc, mb, chanMgr, hot)
	case "feishu":
		return registerFeishuChannels(cc, mb, chanMgr, hot)
	}
	return nil
}

// register adds an adapter to the manager via the appropriate path
// (boot-time Register vs hot RegisterAndStart). Keeps the per-channel
// case branches tidy.
func register(chanMgr *channels.Manager, ch channels.Channel, hot bool) {
	if hot {
		chanMgr.RegisterAndStart(ch)
		return
	}
	chanMgr.Register(ch)
}

func decodeChannelConfig(rec store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	return cc
}

func registerTelegramChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		tg, err := channels.NewTelegram(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		register(chanMgr, tg, hot)
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
		register(chanMgr, tg, hot)
	}
	return nil
}

func registerDiscordChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		dc, err := channels.NewDiscord(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		register(chanMgr, dc, hot)
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
		register(chanMgr, dc, hot)
	}
	return nil
}

func registerSlackChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		sl, err := channels.NewSlack(chCfg.BotToken, chCfg.AppToken, "", mb)
		if err != nil {
			return err
		}
		register(chanMgr, sl, hot)
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
		register(chanMgr, sl, hot)
	}
	return nil
}

func registerLINEChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	// LINE row carries one or more (channel_access_token, channel_secret)
	// pairs keyed by bot userId. AccountConfig.BotToken is the channel
	// access token; AccountConfig.UserID is the channel secret (used
	// for inbound HMAC verification — see channels/line.go field-mapping
	// note).
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		ln, err := channels.NewLINE(token, acct.UserID, accountID, mb)
		if err != nil {
			return err
		}
		register(chanMgr, ln, hot)
	}
	return nil
}

func registerFeishuChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	// Feishu is multi-account: one row carries one or more (app_id,
	// app_secret, verification_token) triples keyed by app_id. No
	// legacy single-bot fallback — the per-account map is the only
	// shape produced by the connect handler.
	for accountID, acct := range chCfg.Accounts {
		secret := acct.BotToken
		if secret == "" {
			secret = chCfg.BotToken
		}
		// AccountConfig.UserID carries the verification token (see
		// channels/feishu.go for the field-mapping rationale).
		lk, err := channels.NewFeishu(accountID, secret, acct.UserID, accountID, mb)
		if err != nil {
			return err
		}
		register(chanMgr, lk, hot)
	}
	return nil
}

func registerWeChatChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	// WeChat is multi-account by design — every QR scan mints a new
	// (botToken, ilink_user_id, baseURL) triple keyed under a fresh
	// accountID. The legacy "no Accounts map → single bot from
	// top-level BotToken" shape doesn't apply (we never have a
	// usable top-level config; the per-account fields BaseURL +
	// UserID are required). So skip the empty-Accounts fallback.
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		wc, err := channels.NewWeChat(token, acct.BaseURL, acct.UserID, accountID, mb)
		if err != nil {
			return err
		}
		register(chanMgr, wc, hot)
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
