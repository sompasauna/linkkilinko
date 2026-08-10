# linkkilinko

`linkkilinko` is a Go moderation bot for Telegram groups and supergroups. It is
designed to:

1. Replace `share.google.com` and `amp.google.com` wrapper links with direct
   destination URLs while attributing the original sender.
2. Require link-only posts to have useful preview metadata or explanatory text.
3. Prevent members who joined less than 48 hours ago from posting links or media.

The initial executable implementation is in `cmd/linkkilinko`. Read
[SPEC.md](./SPEC.md) for the complete behavior, Telegram constraints,
architecture, security model, and acceptance criteria.

## Development

Prerequisites are Go 1.26.5+, `golangci-lint`, and `govulncheck`.

```bash
make build
make test
make test-race
make check
```

Copy [config.example.yaml](./config.example.yaml), set the token through
`LINKKILINKO_TELEGRAM_TOKEN`, select the moderated chat ids, and run:

```bash
go run ./cmd/linkkilinko -config config.yaml
```

The current slice handles text and caption policy actions, copies supported
rewritten media captions, persists moderation outbox work, and retries failed
Telegram side effects. Unsupported or protected media falls back to text.
Optional local health endpoints can be enabled in configuration. 

PS. Never commit a Telegram bot token or runtime database.
