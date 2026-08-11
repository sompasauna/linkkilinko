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

Telegram clients recognize and preview a bare address like
`github.com/owner/repository` even with no `http://` or `https://` typed, and
the Bot API reports it as a `url` entity the same as an explicit link. The bot
follows that recognition: the visible text stays exactly as sent, and
`https://` is used only internally to resolve, fetch, and fingerprint it, so
the scheme-less and explicit HTTPS spellings of the same address are moderated
identically. An explicit non-HTTP link, such as `ftp://` or a Telegram deep
link, is never reinterpreted this way.

### Why Not Use a Hosted Bot?

A search did not find a hosted bot that does this particular job. There are
hosted bots that provide URL blacklisting, but using a closed-source third-party
bot may be worse for privacy: instead of users occasionally clicking a tracking
URL, the group would send all of its messages, usernames, and other metadata to
a bot that must be made an administrator.

Hosted bots also tend to bring in other bells and whistles that make a group
look like a Christmas tree for robots and less like a human conversation.
Running linkkilinko yourself keeps the moderation code and the data path under
the group's control.

### Newcomer sandbox

The 48-hour window replaces the separate Daysandbox bot. Link and media spam in
open Telegram groups or other hit-and-run activity is overwhelmingly posted by
accounts that join and post within minutes, so withholding links and media from
accounts younger than 48 hours in the chat removes most of it without needing
to classify content. Real newcomers lose very little — they can talk from the
first minute, and the restriction lifts on its own.

Join times are recorded from `chat_member` updates and persisted, because
Telegram reports a member's current status but not when they joined.

## Setup

The bot must be an **administrator with permission to delete messages** in every
group it moderates. Without that, it cannot act, and group approval is refused.

A group becomes active through **owner bootstrap**: the first private `/start`
the bot receives registers that sender as the durable owner; later claims from
anyone else are rejected. When that owner then adds the bot to a group and
promotes it to administrator, the bot verifies its delete permission and
approves the group automatically. Approval is stored in SQLite and survives
restarts. Groups added by anyone else stay inert.

There are no commands in groups. Private `/start` is the only interaction.

### Operator Recovery

The owner is durable: once registered, it is never replaced by another
`/start`. Two scenarios lock the deployment out without an explicit recovery
path:

1. The wrong user claims the bot first.
2. The owner account is deleted or otherwise unreachable.

Two CLI flags operate directly on the SQLite database and exit without
starting the bot, gating access by the same filesystem boundary that
already protects the configuration file:

```bash
go run ./cmd/linkkilinko -config config.yaml -reset-owner
go run ./cmd/linkkilinko -config config.yaml -approve-chat -100200300400500
```

`-reset-owner` clears the persisted owner so the next private `/start`
re-bootstraps. Approved chat rows are not touched, so every group the bot
was already trusted to moderate stays moderated.

`-approve-chat <id>` seeds a chat as approved using the existing owner as
the approver. The chat is moderated on the next normal start; no Telegram
interaction is required. The flag refuses to run when no owner is
registered, since there is then no one who could have approved it.

Both flags emit a `slog.Warn` line with the previous and new state so the
recovery action is visible in operational history.

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

## Observability

Every message that contains URLs produces one structured `moderation decision`
log line at INFO (or WARN for fail-open outcomes), plus one `url reasoning`
line per URL carrying the resolved resolver name, destination host, metadata
provider, useful/inconclusive result, fetch error class, and outcome. The
summary line carries `chat_id`, `thread_id`, `message_id`, `sender_id`,
`url_count`, `link_only`, `preview_options`, `preview_disabled`, `multi_link`,
`outcome`, `rule`, and `duration_ms`. `subsystem=moderation` is set on every
line so an operator can filter one message across the Telegram adapter,
resolver, metadata, store, and outbox stages.

Logged moderation URLs retain their scheme, host, and path for diagnosis;
query values and credentials are redacted. Bot tokens, response bodies,
and sender-controlled message text are also out of bounds for logging.

When a message is reported with `preview_disabled=true` and the bot appears
not to rewrite or delete it, check the `outcome` field on the terminal line
and the corresponding `url reasoning` lines: a `fail_open` outcome means
metadata could not be fetched within the configured budget, while a missing
`preview_disabled` entry on the URL entity is a Telegram client limitation.

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
