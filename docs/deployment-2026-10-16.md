# Telegram agreement deployment · 16 October 2026

## Completed

- App `2a58f2d`: release APK built and installed on Pixel 7 using `adb install -r`;
  existing app data preserved. Static analysis and formatting clean; 253 app tests pass.
- Live server checkout `/home/aa/aa-server` force-synced to origin/main `eb6d60f`
  after backing up the prior source, working-tree mode change, configuration,
  database, encryption key, and runtime image. Only the executable mode of
  `install.sh` differed before synchronization; that difference is in the backup.
- Updated sessions container built and deployed. Sessions and Nginx health checks
  pass. Existing network settings and admin credentials are unchanged.
- Public HTTPS checks at `https://aa.yellowteacrew.ru`: health OK, all 12 accounts
  preserved, anonymous pass numbers omitted, unauthorized pass-token RPC rejected
  with HTTP 403. The separate daily-admin testing capability was also verified
  without printing its password or pass numbers.
- Telegram token configured in the private root-owned deployment `.env`; no bot
  or sudo credentials are in source or commits.

## Backup

Root-only directory on the server:
`/home/aa/server-backups/telegram-20261016-1792102272`

It contains `source.bundle`, `source.tar.gz`, `working-tree.patch`, `server.env`,
`sessions.sqlite3`, `master.key`, and the retained Docker image tag. The SQLite
integrity check returned `ok` with 12 accounts. Directory permissions are 700;
backup files are 600. Preserve the database and encryption key together.

Retained image: `aa-server:before-telegram-1792102272`.

## Remaining blocker

The host and application container cannot reach `api.telegram.org`. DNS resolves
its IPv4 address, but direct TCP connections to both `149.154.166.110:443` and
`149.154.167.220:443` timed out; the IPv6 route is unavailable. Telegram `getMe`
cannot finish, so the bot is **not yet operational**. The server's normal pass
grant flow stays closed; daily-admin testing remains available.

The user's Cloudflare domain proxy covers incoming traffic and does not provide
an outbound Telegram route. An authenticated, bot-specific Worker relay is
prepared under `integrations/telegram-relay`; deployment needs the user's
Cloudflare account access or the URL and shared secret of a Worker they deploy.
Its code does not require DNS or server-wide routing changes. Live owner/login,
request, approval and revocation checks remain pending until that route works.

## Prepared relay validation

The optional relay and server configuration pass 92 Python tests and four Node
Worker tests. The relay source is ready to publish; no Cloudflare account access
is connected. The live container remains on the already deployed core `eb6d60f`
until the relay URL and secrets can be configured. The install script's executable
bit is now tracked in Git, preserving the intent of the mode change found on the
remote checkout; `bash -n` and `./install.sh --check` pass.
