# Telegram Plugin

Telegram bot channel plugin for FastClaw.

## Install

```bash
fastclaw plugin install telegram
```

## Configuration

Add your bot token in `~/.fastclaw/fastclaw.json`:

```json
{
  "plugins": {
    "entries": {
      "telegram": {
        "enabled": true,
        "config": {
          "botToken": "YOUR_TELEGRAM_BOT_TOKEN"
        }
      }
    }
  }
}
```

Get a bot token from [@BotFather](https://t.me/BotFather) on Telegram.

## Status

🚧 Work in progress — currently Telegram is built into the FastClaw binary. This plugin will replace the built-in implementation.
