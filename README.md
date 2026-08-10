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

This project is dedicated to the public domain under [CC0 1.0](./LICENSE).
Operators may overlay the selected notice catalog with
`moderation.notice_catalog: /path/to/catalog.yaml`; unspecified keys retain
the embedded Finnish catalog text.

Unknown keys, empty values, malformed YAML, and placeholders unsupported by the
notice key fail startup.

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
