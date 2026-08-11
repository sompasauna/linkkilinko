# linkkilinko Specification

Status: initial product and architecture specification for v0.1.

`SPEC.md` is the source of truth for user-visible behavior. If implementation
and this document disagree, either the implementation must change or this
document must be updated deliberately.

## Summary

`linkkilinko` is a moderation bot for Telegram groups and supergroups. It keeps
shared links direct and understandable and replaces the separate Daysandbox bot
with a 48-hour anti-spam rule.

The bot has three core responsibilities, evaluated in this order:

1. Block links and media from human members who joined the chat less than 48
   hours ago.
2. Replace Google sharing and AMP wrapper URLs with their direct destinations,
   and remove known tracking parameters from shared URLs.
3. Moderate link-only messages that do not provide useful preview metadata or
   explanatory text.

The first materially distinct violation produces one visible explanation or
replacement. Reposting the same violating content without a material change
does not produce another bot response: the repost is deleted silently and the
first response remains the canonical visible action.

The bot ignores its own messages, so replacements and notices never enter a
moderation loop.

## Goals

1. Remove `share.google` and legacy `goo.gl` redirects, Google AMP-cache URLs,
   and recognizable publisher AMP URLs from messages and publish direct
   destination URLs instead.
2. Require a link-only post to be understandable from metadata or from text
   written by the sender.
3. Prevent newly joined members from posting links or media for exactly 48 hours.
4. Preserve attribution whenever the bot replaces a user's message.
5. Avoid flooding the chat with repeated responses to unchanged reposts.
6. Keep URL resolution and future URL policy checks extensible without a chain
   of hostname substring conditionals.
7. Make every moderation decision observable, idempotent, durable, and testable.
8. Fetch untrusted URLs without exposing the host or internal network to SSRF.

## Non-goals For v0.1

1. General spam, toxicity, or malware classification.
2. A scam blocklist. The architecture provides a policy-check stage where one
   can be added later.
3. URL shortening or broad removal of arbitrary, unrecognized query parameters.
4. Editing messages sent by users; ordinary Bot API group messages cannot be
   edited by the bot.
5. Rehosting arbitrary remote images or videos to manufacture rich previews.
6. Moderating private chats, broadcast-channel posts, or messages sent by other
   bots.
7. An administrator dashboard. Configuration is file-based for v0.1.

## Telegram Platform Constraints

The design uses the Telegram Bot API and must not assume client-only or MTProto
capabilities.

1. The bot cannot edit an ordinary group message sent by a user. Rewriting is
   implemented as resolve first, delete the original, then send a bot-authored
   replacement.
2. The bot must be an administrator with permission to delete messages. Telegram
   only permits deletion within 48 hours, which is sufficient because moderation
   is immediate.
3. The bot must receive all group messages. Administrator bots receive them; if
   deployment changes that assumption, Group Privacy Mode must be disabled.
4. The update subscription must explicitly include `chat_member`; it is excluded
   from the default update set. The bot also consumes `message` and
   `edited_message`.
5. `ChatMemberUpdated.date` is the authoritative observed join time. The
   `getChatMember` method reports current status but not historical join time, so
   join timestamps must be persisted locally.
6. Incoming Bot API messages do not include the rendered link-preview payload.
   `link_preview_options` only reports changed generation options, such as an
   explicitly disabled preview. Therefore v0.1 uses a bot-owned metadata probe as
   the operational definition of whether useful preview data is available.
7. Replacement messages must stay in the original forum topic by carrying the
   original `message_thread_id` where applicable.

References:

1. [Telegram Bot API](https://core.telegram.org/bots/api)
2. [Telegram Bots FAQ: messages visible to bots](https://core.telegram.org/bots/faq)
3. [Telegram Bot Features: Group Privacy Mode](https://core.telegram.org/bots/features#privacy-mode)

## Terminology

### Link

An HTTP or HTTPS URL found in a Telegram `url` or `text_link` entity in message
text or a media caption. Entity-aware extraction is authoritative; raw regular
expressions are not the main parser.

A `url` entity's visible text is recognized even without an explicit `http://`
or `https://` prefix, matching Telegram clients' own link detection for text
such as `github.com/owner/repository`. The unmodified visible text remains
what the bot displays and replaces; `https://` is prefixed only to build the
canonical target used for resolution, metadata retrieval, policy checks, and
fingerprinting, so a scheme-less spelling and its explicit HTTPS spelling are
treated as the same link throughout. An explicit non-HTTP scheme, such as
`ftp://` or a Telegram deep link, is rejected outright and is never
reinterpreted as HTTPS. This recognition does not extend to `text_link`
entities, whose `url` field is always an explicit destination set by the
sender.

### Link-only message

A text message whose content, after removing all URL entity spans, contains only
Unicode whitespace or punctuation. A captioned media message is not link-only;
it is media and is only affected by the newcomer sandbox and tracked-link
rewriting.

Messages containing an explanation alongside a URL are not subject to preview
moderation. They remain subject to the newcomer sandbox and tracked-link rules.

### Media

Any Telegram message containing a photo, video, animation, audio, voice note,
video note, document, sticker, story, paid media, or a media group identifier.
Contacts, locations, venues, polls, dice, and service messages are not media for
the 48-hour rule in v0.1.

### Useful preview metadata

At least a non-empty human-readable title, or a site name plus a non-empty
description, obtained from a successful bounded metadata probe. A URL, HTTP
status, MIME type, or image alone is not sufficient.

Title provenance matters. A title lifted from the Open Graph or Twitter card
property is authoritative for the link. A title lifted from the HTML
`<title>` element is the fallback used only when no Open Graph or Twitter
card title is present, and it counts as useful only when:

1. it is accompanied by a non-empty description, or
2. it differs meaningfully from the site's `og:site_name`, the registrable
   domain, or the URL host. A `<title>` that is just the registrable domain,
   the `og:site_name`, or the bare host is not evidence about the link and
   does not satisfy the rule on its own.

This keeps the `<title>` fallback useful for ordinary pages without Open
Graph tags, while excluding login walls, cookie-consent interstitials,
paywalls, and similar challenges whose only metadata is a bare site-name
`<title>`.

### Materially identical repost

A new message from the same sender in the same chat and forum topic whose
normalized moderation input and applicable moderation rule match an active
canonical action.

Normalization includes:

1. Unicode normalization, normalized line endings, and insignificant surrounding
   whitespace.
2. Parsed and normalized URL scheme and host, default-port removal, and preserved
   meaningful path, query, and fragment data.
3. Telegram media `file_unique_id`, media kind, and normalized caption.
4. The moderation rule and its behavior/configuration version.

Adding meaningful explanatory text, changing a destination, changing the media,
or changing the applicable rule is a material change. Cosmetic whitespace or an
equivalent spelling of the same URL is not. Fingerprints are sender-specific;
one user's response must not silently stand in for a different user.

## Configuration

Configuration is YAML. Secrets may also be supplied through environment-backed
secret injection; the token must never be committed.

```yaml
telegram:
  token: ""

database:
  path: "/var/lib/linkkilinko/linkkilinko.sqlite"

moderation:
  newcomer_sandbox: 48h
  notice_language: "fi"
  # Optional YAML overlay; embedded catalog entries remain the defaults.
  notice_catalog: ""

metadata:
  request_timeout: 5s
  total_timeout: 10s
  max_redirects: 5
  max_html_bytes: 2097152
  user_agent: "linkkilinko/0.1"
```

Required validation:

1. The token is non-empty after secret resolution.
2. The newcomer duration is positive and defaults to exactly 48 hours.
3. Network limits are positive and remain within compiled safety ceilings.
4. The database directory exists and is writable, or can be created safely.
5. `notice_language` is non-empty and selects a compiled-in message catalog that
   defines every notice key policy can emit. Startup fails otherwise.

If `notice_catalog` is configured, it is a YAML overlay keyed by notice key.
Unspecified keys retain the embedded catalog text. Unknown keys, empty values,
malformed YAML, and placeholders unsupported by the notice key fail at startup.

User-visible notice and replacement text is not written inline in policy or
transport code. Policy emits a notice key plus parameters; a per-language
catalog owns the wording quoted throughout this document, and parameters are
substituted literally so sender-controlled text can never introduce markup or a
further placeholder.

## Moderation Pipeline

Each incoming or edited human-authored message is converted into a transport-free
domain input. Policy evaluation returns a plan; it never calls Telegram, HTTP,
or SQLite directly.

The stages are ordered and short-circuit as follows.

### 1. Scope And Update Idempotency

1. Ignore chats that have not been approved by the owner through the bootstrap
   flow.
2. Ignore bot-authored messages, service messages, and messages with no stable
   positive message id.
3. Claim `(chat_id, message_id, edit_date)` in persistent idempotency storage.
4. A redelivered update returns the previously recorded terminal result rather
   than producing another replacement or notice.

Update idempotency handles delivery of the same Telegram message more than once.
Repost suppression, described below, handles a new message id carrying unchanged
content.

### 2. Canonical Action And Silent Repost Suppression

Before any DNS lookup, HTTP request, resolver, metadata provider, or new visible
Telegram action, compute the normalized source fingerprint and look for an active
canonical action from the same sender, chat, topic, and behavior version.

Time-dependent applicability is revalidated locally. For example, a canonical
newcomer action matches only while the sender is still under 48 hours. A stored
resolver or preview action can match without repeating its external lookup
because identical normalized input and behavior version already produced the
canonical result.

For a later new message with the same active fingerprint:

1. Delete the new user message.
2. Do not resolve the URL again, fetch metadata again, send another notice,
   send another replacement, edit the first response, or create another outbox
   action.
3. Record the silent deletion against the canonical action for auditing.
4. Keep the original bot response as the only visible explanation.
5. If the original response is still pending because its send failed after
   deletion, continue retrying that original outbox item; do not create a second
   one.

The silent deletion is itself a required Telegram operation, but it creates no
new user-visible action. A delete failure is logged and retried according to the
normal action state machine.

If there is no active match, evaluation continues. When a later stage first
deletes or replaces a materially distinct message, the bot persists its
fingerprint and canonical action before deleting the source. The canonical
action points to the single visible bot response and its outbox state.

Canonical fingerprints suppress reposts for four hours from the original
moderation action. This window is evaluated from durable creation time, so it
survives process restarts without suppressing test messages or stale decisions
indefinitely. After the window, the message is re-evaluated and may produce a
new canonical action. A fingerprint also stops suppressing sooner when the
applicable rule no longer rejects or replaces the message, when normalized
content materially changes, or when a behavior/config version invalidates the
old decision. In particular, a newcomer fingerprint no longer applies once the
sender reaches 48 hours, because the same post is then re-evaluated as allowed
by that rule.

### 3. Newcomer Sandbox

For a human sender with a known most-recent join time:

1. Compute age using an injected UTC clock and the persisted Telegram event
   timestamp.
2. If age is less than 48 hours and the message contains any link or media,
   delete it immediately and post the Finnish sandbox notice unless the same
   canonical violation already has a visible response.
3. At exactly 48 hours the restriction expires.
4. A leave followed by a rejoin records a new join time and starts a new window.
5. Administrators are not exempt merely because they are administrators.

The sandbox short-circuits URL resolution and metadata retrieval.

Notice text:

> Alle 48 tuntia kanavalla olleet eivät voi lähettää mediaa tai linkkejä. Tällä
> estetään spämmäystä. Jos asia on tärkeä (kuvat kadonneista esineistä jne.),
> joku kanavalla kauemmin ollut voi auttaa välittämään sen.

Unknown join times are grandfathered as established members instead of blocking
an entire existing group for 48 hours on first deployment. The bot records this
state and logs a warning. This is an explicit fail-open compromise: downtime can
create a gap that the Bot API cannot reconstruct later. Continuous operation and
durable membership storage are required for full Daysandbox parity.

### 4. Tracked-Link Rewriting

If any extracted URL has an exact normalized host of `share.google` or `goo.gl`,
matches a Google or AMP Project cache URL shape, is recognized as a
publisher-hosted AMP URL by a registered rule, or contains a known tracking
parameter:

1. Resolve every matched URL before deleting anything.
2. Accept only a valid public HTTP or HTTPS destination outside the matched
   wrapper host that passes all URL safety checks. Parameter removal is a
   deterministic local transformation and does not require a network request.
3. Replace only the URL entity span. Preserve surrounding user text and safe
   formatting entities where possible.
4. Delete the original only after the complete replacement payload and durable
   outbox record are ready.
5. Send the replacement in the same chat and forum topic with an entity-based
   mention of the original sender.
6. Run link-only preview policy against the resolved destination before final
   rendering, so a wrapper link cannot bypass preview requirements.

Replacement introduction:

> Linkki, joka kerää käyttäjien tietoja mainontaa ja seurantaa varten, korvattiin
> suoralla linkillä.

The replacement identifies the sender and reproduces the original content with
the direct URL. For captioned media supported by Telegram `copyMessage`, the bot
copies the media with a rewritten caption. If content protection or media type
prevents copying, the safe fallback is a text replacement containing the
attribution, direct URL, and original caption. Protected media is not downloaded
and reuploaded.

If resolution is inconclusive because of a timeout, temporary upstream failure,
unsafe redirect, or malformed response, the bot leaves the original untouched
and logs the failure. It must never delete first and hope resolution succeeds.

### 5. Link-only Preview Policy

This stage applies only to link-only messages after tracked-link rewriting.

1. Probe each URL through the metadata subsystem.
2. If the user explicitly disabled Telegram previews and useful metadata is
   available, keep the original and reply to it with only the fetched metadata.
3. If a successful, definitive probe finds no useful metadata, delete the
   original and post the explanatory-text notice.
4. If useful metadata is available and the preview was not explicitly disabled,
   leave the original message in place. Telegram is assumed to render its native
   preview.
5. If the probe fails transiently or is rejected for network safety, leave the
   original in place and log the reason. Infrastructure failure is not proof
   that a page lacks metadata.

Metadata reply template:

> {site name, when available}
> {title}
> {description, when available}

No-metadata notice:

> Linkistä ei ollut saatavilla esikatselutietoa. Voit lähettää linkin uudelleen,
> mutta kerro mitä linkin takaa löytyy.

Telegram does not expose whether a native preview silently failed when the user
did not disable it. Consequently v0.1 can miss the narrow case where the bot can
fetch metadata but Telegram cannot render it. Provider-specific preview rules or
an MTProto sidecar may address this later; the implementation must not pretend
the Bot API exposes data it does not.

For multiple URLs, the message is acceptable only if every URL has useful
metadata and previews were not explicitly disabled. An enriched replacement
renders compact metadata for each URL in original order within Telegram limits.

## URL Extension Architecture

URL handling is a pipeline of registered components, not a chain of
`strings.Contains` checks.

### Domain Types

The core link package owns transport-free values similar to:

```text
URL                 parsed URL plus normalized scheme and host
Resolution          original URL, final URL, rule name, redirect evidence
Metadata            canonical URL, site name, title, description
Verdict             allow, replace, reject, or unknown plus reason and source
```

### Resolver Registry

Each resolver has a stable name, an explicit matcher, and a resolve operation:

```text
Name() string
Match(URL) bool
Resolve(context, URL) (Resolution, error)
```

The registry evaluates resolvers in declared priority order and rejects
ambiguous equal-priority matches during startup. Host matchers operate on parsed,
lowercased, IDNA-normalized hosts and support exact host or explicit subdomain
rules. They never use arbitrary substring matching.

The initial registry contains:

1. `google-share`: exact hosts `share.google` and legacy `goo.gl`; follows
   bounded redirects to a non-wrapper destination.
2. `amp`: recognizes Google `/amp/` cache URLs, `*.cdn.ampproject.org` cache
   URLs, `amp.` publisher subdomains, and explicit AMP path or query markers.
   It unwraps deterministic cache paths without a request; otherwise it safely
   fetches the page and accepts a non-AMP HTML canonical URL. Inconclusive
   heuristic rewrites are not accepted merely because an AMP-looking path can
   be stripped.
3. `tracking-parameter`: removes conservative generic parameters such as
   `utm_*`, `fbclid`, `gclid`, and `ttclid`, plus exact host rules for parameters
   such as YouTube `si`, `pp`, and `s`, IS.fi `shem`, Spotify `si`, and Instagram
   `igshid`. Meaningful parameters such as YouTube `v`, `t`, `list`, and `index`
   are preserved. The rule is local and deterministic.

Safe generic redirects and metadata retrieval are provided by the hardened
fetcher, not by a catch-all resolver. A resolver that matches every URL would
make wrapper detection ambiguous.

### Metadata Providers

Metadata providers use the same registry pattern. The generic HTML provider
extracts, in priority order:

1. Open Graph title, description, site name, and canonical URL.
2. Twitter card title and description.
3. HTML `<title>` and standard description metadata.

Provider-specific adapters, such as a future Facebook adapter, can match an
exact host family and use a supported API or tailored parser without changing
moderation policy.

### Future Policy Checks

A separate checker registry runs after resolution and before preview fetching:

```text
Name() string
Check(context, URL) (Verdict, error)
```

A future scam blocklist is a checker, not a resolver. This keeps URL
transformation, metadata retrieval, and allow/reject policy independently
testable. v0.1 ships with no external blocklist checker.

## Metadata Retrieval And SSRF Safety

All resolvers and metadata providers use one hardened fetch service; direct use
of the default `http.Client` elsewhere is forbidden.

Required controls:

1. Permit only `http` and `https`; reject credentials, malformed ports, and
   non-canonical hosts.
2. Resolve DNS before connecting and reject every loopback, private, link-local,
   multicast, unspecified, benchmark, documentation, carrier-grade NAT, and
   otherwise non-public address.
3. Dial only an address from the validated set and keep TLS hostname validation
   bound to the original hostname. Revalidate every redirect target and DNS
   result to defend against DNS rebinding.
4. Reject redirects to disallowed schemes, credentials, or non-public networks.
5. Enforce connect, response-header, per-request, total-operation, and maximum
   redirect limits.
6. Read through a hard byte limit and abort oversized bodies. Parse only HTML or
   explicitly supported textual content types.
7. Send no Telegram token, cookies, authorization headers, or internal headers.
8. Do not execute JavaScript or honor HTML refresh redirects in v0.1.
9. Log normalized hosts and outcomes, not response bodies or sensitive query
   values.

Network-safety rejection is an operational error and fails open for moderation;
it is not evidence that preview metadata is absent.

## Persistent State

SQLite is the durable source of observed membership history, update idempotency,
canonical repost suppression, and in-flight actions. At minimum it stores:

```text
memberships(chat_id, user_id, joined_at, status, observed_at, grandfathered)
processed_updates(chat_id, message_id, edit_date, state, decision, updated_at)
canonical_actions(id, chat_id, thread_id, user_id, rule, behavior_version,
                  fingerprint, response_message_id, response_state, created_at)
suppressed_reposts(canonical_action_id, source_message_id, delete_state,
                   observed_at, updated_at)
outbox(id, canonical_action_id, chat_id, thread_id, source_message_id,
       payload, state, attempts, next_attempt_at)
```

Membership transitions:

1. A transition from non-member to member, restricted-member, administrator, or
   owner records `joined_at` from `ChatMemberUpdated.date`.
2. `new_chat_members` service messages are a fallback and do not overwrite an
   earlier timestamp for the same continuous membership period.
3. Leaving or being banned ends the membership period.
4. Rejoining starts a new period even if old history exists.
5. Startup migrations run transactionally before update consumption begins.

The consumer does not advance durable progress past a membership transition
until its database write succeeds.

## Telegram Actions And Failure Handling

Destructive moderation cannot be a database transaction with Telegram. The bot
uses a durable state machine:

```text
observed -> planned -> delete_requested -> deleted -> send_pending -> complete
                                      \-> delete_failed
                           send_pending -> retry_wait -> complete/dead_letter
```

Rules:

1. Resolve and render before deletion.
2. Persist the canonical plan, fingerprint, and replacement payload before
   deletion.
3. Never send a replacement if deletion definitively failed; that would
   duplicate the original.
4. After successful deletion, retry transient send failures with bounded
   exponential backoff and Telegram `retry_after` handling.
5. A permanent send failure after deletion enters dead-letter state and emits a
   high-severity structured log suitable for operator alerting.
6. Telegram Bot API calls do not provide an idempotency key. If the process
   crashes after Telegram accepts a send or media copy but before SQLite records
   the returned message ID, recovery may repeat that one side effect. The
   durable state machine minimizes this window but cannot eliminate it without
   a separate reconciliation client.
7. Notices and replacements use explicit Telegram entities rather than
   parse-mode interpolation. User-controlled names, text, and URLs do not become
   markup.
8. Mentions use the sender's user id and a safe display label; usernames are not
   required.
9. Edited messages are re-evaluated under a new edit version. Editing explanatory
   text away, adding a tracked link, or adding a link while sandboxed triggers
   the same policy and repost fingerprint rules.

## Observability And Privacy

Each decision log includes chat, topic, message, and sender ids; rule and resolver
names; membership-age classification; normalized source and destination hosts;
canonical action id; whether deletion was a silent repost suppression; outcomes,
retry count, error class, and duration.

Logged URLs retain scheme, host, and path for diagnosis; query values and
credentials are redacted. The bot stores only data
needed for membership enforcement, idempotency, canonical responses, retries,
and operator diagnosis. Message bodies are removed after fingerprinting and
terminal outbox completion unless needed for the active canonical payload.

## Testing Requirements

### Core Policy And Fingerprints

1. Boundary cases at 47h59m59s, exactly 48h, and after 48h.
2. Join, leave, rejoin, grandfathered, administrator, bot, anonymous sender, and
   unknown-membership cases.
3. Every media type and URL entity type.
4. Text-plus-link versus link-only classification, including Unicode whitespace,
   punctuation, multiple links, captions, and edits.
5. Stage ordering: newcomer rejection prevents resolver/fetcher calls; tracked
   links are rewritten before preview policy.
6. Same Telegram update is idempotent; a new message id with the same normalized
   violation is silently deleted and refers to the original canonical action.
7. Cosmetic whitespace and equivalent URLs suppress; meaningful text, URL,
   media, sender, topic, rule, and behavior-version changes do not.
8. Restart preserves canonical suppression. Sandbox expiry makes an old
   newcomer fingerprint inapplicable.

### Resolvers, Metadata, And Security

1. Google wrapper redirects, redirect loops, unsafe redirects, relative
   locations, canonical tags, and unresolved wrappers.
2. Exact-host matching proves that `share.google.attacker.example` and
   `goo.gl.attacker.example` do not match; AMP shape tests cover both recognized
   cache/publisher forms and ordinary words containing `amp`.
3. Open Graph, Twitter card, HTML fallback, missing metadata, malformed HTML,
   unsupported content, oversized bodies, timeouts, and temporary failures.
4. Registry ordering, ambiguous registration rejection, and independent future
   checker registration.
5. Private/reserved IP rejection on initial URLs and redirects, DNS rebinding
   defense, body limits, redirect limits, and deadlines.

### Storage And Telegram Workflow

1. SQLite migration and restart persistence.
2. Duplicate and out-of-order updates.
3. Delete failure, send failure after delete, retry, rate limit, dead letter, and
   restart recovery at every durable state.
4. Reposts received while the canonical response is pending reuse that outbox
   item and do not create another one.
5. Replacement attribution and entities resist markup injection.
6. Replacements and notices remain in the original forum topic.

### Live Acceptance

A private Telegram test group demonstrates:

1. A newly joined test user has a link and each media category deleted with the
   sandbox notice.
2. An unchanged repost is deleted silently and the first notice remains the only
   bot response.
3. An established user can post ordinary media and explanatory link messages.
4. `share.google`, legacy `goo.gl`, Google AMP-cache, and publisher AMP examples
   are replaced with verified direct URLs and correct attribution.
5. Reposting either unchanged wrapper message produces no second replacement;
   changing the accompanying text produces a new canonical action.
6. A link-only post with an explicitly disabled preview keeps the original and
   receives a metadata-only reply when metadata is available; otherwise it is
   removed with the no-metadata notice, and an unchanged repost is silent.
7. A restart preserves newcomer age, canonical suppression, and outbox state.
8. Removing delete permission produces a visible operator error and no duplicate
   replacement.

Live tests use a dedicated bot token and group. They never run against the
production group by default.

## Deployment Requirements

1. Run as an unprivileged system user with a private state directory.
2. Inject the Telegram token at runtime; never place it in the repository or
   command-line arguments visible to other users.
3. Grant administrator status and message-deletion permission in every configured
   group.
4. Subscribe to `message`, `edited_message`, and `chat_member` updates.
5. Back up SQLite so active membership windows, canonical responses, and outbox
   actions survive host failure.
6. Expose health for Telegram connectivity, database readiness, consumer
   progress, and outbox backlog.
7. Shut down gracefully: stop intake, persist in-flight decisions, and close the
   database without losing the update offset.

## Acceptance Criteria For v0.1

v0.1 is complete when:

1. All required moderation behaviors work in a real private test group.
2. Unchanged reposts within the four-hour suppression window are deleted
   silently across process restarts and never create a second response or
   outbox action.
3. The Google rules are registry-based; adding a resolver or checker requires
   registration and tests, not a hostname conditional chain.
4. Join state and in-flight replacement state survive restart.
5. Metadata fetching satisfies all SSRF and resource-limit tests.
6. Finnish notices identify the policy clearly and replacements mention the
   original sender safely.
7. The bot never deletes a message before it has a complete durable action plan.
8. `make check` passes.

## Explicit Product Limitation And Follow-up Decision

The Bot API cannot positively report the rendered native preview for an ordinary
incoming message. v0.1 therefore guarantees handling for explicitly disabled
previews and pages definitively lacking metadata under the safe probe, but it
cannot perfectly identify every Telegram-side preview failure.

Before promising perfect equivalence to what a Telegram client visibly renders,
choose one later direction:

1. Keep the v0.1 proxy semantics and document the limitation.
2. Replace every link-only message with a bot-generated metadata summary, even
   when Telegram may already have shown a native preview.
3. Add a separately operated MTProto/TDLib observer after evaluating credential,
   deployment, security, and maintenance costs.

### Decision

For v0.1, retain option 1: use the bounded bot-owned metadata probe as the
preview policy authority. Do not add a TDLib or MTProto observer. A successful
probe with useful metadata, an explicitly disabled preview, and a definitive
no-metadata result remain the supported decisions; Telegram's private native
preview-rendering result is intentionally outside this bot's scope.

Hosts that produce useful metadata are persisted as known-good preview domains.
For later link-only messages with previews enabled, an exact normalized host
match avoids another metadata probe and leaves the message untouched. A cached
host is considered fresh for 30 days after its last live check; stale entries
are probed again. The cache is a performance optimization, not a trust or
safety allowlist: Facebook,
`fb.com`, `share.google`, and `goo.gl` (including their subdomains) can never be
recorded as known-good domains, and tracked-link rewriting still runs first.

## Operator Recovery

The owner is durable: the first private `/start` registers that sender, and
later claims are rejected. Two operational scenarios lock the deployment out
without an explicit recovery path:

1. The wrong user claims the bot first. The bot username is discoverable as
   soon as it is created, and the registration is permanent.
2. The owner account is deleted, banned, or otherwise unreachable. Existing
   approvals keep working, but no new group can ever be approved.

Group moderation stays command-free, and a chat-based reset would be exactly
the attack surface the bootstrap design avoids. Recovery is therefore an
**out-of-band operator action** through two CLI flags that operate directly on
the SQLite database and exit without starting the bot:

1. `-reset-owner` clears the persisted owner so the next private `/start`
   re-bootstraps. Approved chat rows are not affected: every group the bot
   was already trusted to moderate stays moderated.
2. `-approve-chat <id>` seeds a chat as approved using the existing owner as
   the approver. The chat is moderated on the next normal start; no Telegram
   interaction is required. The flag refuses to run when no owner is
   registered, since there is then no one who could have approved it.

Both flags gate on filesystem access to the database, which is the same
trust boundary that editing the configuration file already assumes, and log
the previous and new state at `slog.Warn` so the recovery action is visible
in operational history. Neither flag is reachable through a Telegram
interaction.
