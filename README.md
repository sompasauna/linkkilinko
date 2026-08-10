# linkkilinko

`linkkilinko` is a Go moderation bot for Telegram groups and supergroups. It is
designed to:

1. Replace `share.google` and legacy `goo.gl` redirect links, plus recognized
   Google-cache and publisher AMP URLs, with direct destination URLs while
   attributing the original sender.
2. Require link-only posts to have useful preview metadata or explanatory text.
3. Prevent members who joined less than 48 hours ago from posting links or media.

The initial executable implementation is in `cmd/linkkilinko`. Read
[SPEC.md](./SPEC.md) for the complete behavior, Telegram constraints,
architecture, security model, and acceptance criteria.

This project is dedicated to the public domain under [CC0 1.0](./LICENSE).

## Why These Rules

### Link rewriting

A `share.google` or `goo.gl` link tells the reader nothing about where it goes,
and it routes the click through a redirector that observes it. AMP URLs are the
same problem in a different shape: the address in the chat is not the publisher's
address. Replacing the wrapper with its destination keeps the link both direct
and readable, and it leaves the URL itself as the thing a reader can judge before
clicking.

The bot cannot simply edit the wrapper out. The Bot API does not permit a bot to
edit an ordinary group message written by a user, so rewriting is necessarily
resolve, delete the original, then post a bot-authored replacement. That is why
the replacement always names the original sender: the message has changed hands,
and attribution is what keeps the conversation legible.

### Newcomer sandbox

The 48-hour window replaces the separate Daysandbox bot. Link and media spam in
open Telegram groups is overwhelmingly posted by accounts that join and post
within minutes, so withholding links and media from accounts younger than 48
hours in the chat removes most of it without needing to classify content. Real
newcomers lose very little — they can talk from the first minute, and the
restriction lifts on its own.

The window also fits what Telegram allows: a bot may only delete messages up to
48 hours old, which is ample because moderation happens as the message arrives.

Join times are recorded from `chat_member` updates and persisted, because
Telegram reports a member's current status but not when they joined.

## Setup

The bot must be an **administrator with permission to delete messages** in every
group it moderates. Without that, it cannot act, and group approval is refused.

A group becomes active in one of two ways:

1. **Owner bootstrap.** The first private `/start` the bot receives registers
   that sender as the durable owner; later claims from anyone else are rejected.
   When that owner then adds the bot to a group and promotes it to administrator,
   the bot verifies its delete permission and approves the group automatically.
   Approval is stored in SQLite and survives restarts. Groups added by anyone
   else stay inert.
2. **Configured allowlist.** Chat ids listed under `telegram.allowed_chat_ids`
   are always treated as active. This is checked *before* the persisted
   approvals, so it is an override: a chat id listed there is moderated without
   any owner approval.

**Current limitation:** `allowed_chat_ids` must presently be non-empty or the
bot refuses to start, so owner bootstrap cannot yet be used on its own. Until
that is fixed, `config.example.yaml` ships a placeholder id — note a placeholder
is *not* inert, since any chat matching it is moderated without owner approval.
Pick an id you control.

There are no commands in groups. Private `/start` is the only interaction.

## Configuration

Copy [config.example.yaml](./config.example.yaml) and set the token through
`LINKKILINKO_TELEGRAM_TOKEN`.

Operators may overlay the selected notice catalog with
`moderation.notice_catalog: /path/to/catalog.yaml`; unspecified keys retain the
embedded Finnish catalog text. Unknown keys, empty values, malformed YAML, and
placeholders unsupported by the notice key fail startup, so a typo is caught at
boot rather than sent to a chat.

The sandbox length is `moderation.newcomer_sandbox`, and the metadata probe's
timeouts, redirect limit, and body limit live under `metadata`.

## Development

Prerequisites are Go 1.26.5+, `golangci-lint`, and `govulncheck`.

```bash
make build
make test
make test-race
make check
```

```bash
go run ./cmd/linkkilinko -config config.yaml
```
