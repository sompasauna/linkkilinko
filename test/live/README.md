# Private Telegram acceptance test

Run this only in a disposable private supergroup. Do not use production chat
IDs or credentials.

1. Create a temporary bot, grant it administrator permission to delete messages,
   and set `LINKKILINKO_TELEGRAM_TOKEN`.
2. Configure the test supergroup ID and a writable temporary SQLite path.
3. Start linkkilinko with `operational.health_listen` empty unless a local
   readiness probe is wanted.
4. Verify an established member can post ordinary media and explanatory links.
5. Add a fresh test account and verify links, photos, videos, documents, and
   captions are removed with the quarantine notice.
6. Post `share.google.com` and `amp.google.com` examples; verify direct URLs,
   attribution, and forum-topic placement.
7. Repeat each unchanged violation; verify silent deletion and no second bot
   response.
8. Restart the process and repeat a suppressed repost and a pending outbox case.
9. Remove the bot's delete permission; verify the original is not replaced and
   the action is logged as failed or dead-lettered.

Record the Telegram message IDs and observed outcomes in the issue or release
notes, without recording tokens or private message bodies.
