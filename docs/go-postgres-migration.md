# Go / PostgreSQL migration status

The Go implementation covers all 97 in-scope HTTP routes; the three Telegram
routes are deliberately excluded. PostgreSQL is the only runtime database.
`Dockerfile.native`, `compose.go.yaml`, `install-go.sh` and `migrate-go.sh` provide
the native deployment. The original Python deployment remains available for
rollback. No production server or database has been accessed.

## Implemented

- PostgreSQL runtime storage for accounts, encrypted credentials, sessions,
  sharing, marks, audit and ordered event synchronization, with bounded pools.
- Compatible Fernet using the preserved master key, scrypt hashes, Ed25519
  signing seeds/JWTs, daily codes and native gRPC-Web requests/parsers.
- Account import, metadata, permissions, sharing/request decisions, settings,
  owner changes, panel login/CSRF, site login/callback and static assets.
- AA ID device authentication, refresh rotation/reuse revocation, web sessions,
  one-time site codes, account proxies and device management.
- Native Playwright QR login and silent-renewal primitives with separate browser
  budgets. QR login is connected to AA ID and panel enrollment. Responses are
  disposed after reading; cancellation closes contexts; expired jobs are removed
  periodically; AA ID claims atomically remove retained credentials.
- Moscow calendar recurrence/exclusions, class matching, group search and both
  schedule APIs. Bounded caches coalesce downloads and preserve saved calendars
  during failures, following each service's existing refresh semantics.
- Read-only SQLite snapshot export and atomic import into an empty PostgreSQL
  target, with checksum/count checks and rollback. Keys are never regenerated.
- Account checks and automatic browser renewal, coalesced per account, with
  bounded workers, cancellation, retry backoff and shutdown draining.
- Site lesson rosters/marking, attendance and turnstile views, journal aggregation
  and panel diagnostics. Cached site data and browser work have explicit budgets.
- Separate database roles and separate read-only key mounts for the main service
  and AA ID; applications run without root and with a read-only container root.

Telegram is disabled in Go at the user's request: no linking, bot, legacy grants
or enrollment page. Its old tables remain inert for snapshot import and rollback;
no imported Telegram grant authorizes an API operation. AA ID sharing remains.

The [generated route report](go-route-coverage.json) excludes three Telegram routes.
Registered handlers are not a measure of total functionality
or proof of behavioral parity.

## Mandatory access policy

The user explicitly requested tighter authorization during migration. These are
intentional changes from the old optional-auth behavior:

- Every `/api/*` and `/site/*` request requires a verified AA ID bearer or valid
  site session. No verifier means anonymous requests still fail closed. Invalid
  bearers cannot fall back to cookies. The executable requires AA ID URLs.
- Panel APIs require a panel session; changes also require its CSRF token.
- Snapshots/events select only the owner and explicitly shared enabled passes
  in SQL. Group IDs, test passwords and device labels do not grant access.
- Shared pass access does not authorize profile/ACS/attendance RPCs, account
  changes or session termination. Foreign/missing account IDs return the same 404.
- Imported MIREA sessions must match the signed-in identity. Pass-number reads
  check current permissions again. Shared cards omit authentication timestamps;
  version counters required by the app remain present. Selected-group filters
  apply to owned and shared cards. Access revocation emits a private event so
  clients refresh even after the revoked card becomes invisible.
- Person discovery requires a connected account and an exact username, with
  20 searches per minute. General API limits are 300 reads and 60 writes per
  minute per identity, with `Retry-After` on rejection.

Bootstrap/login pages, static assets, minimal health/JWKS discovery, enrollment
capabilities and signed internal handoff retain the credentials required by their
flows. JWT validation remains local with its existing expiry window; immediate
global revocation and shared quotas across replicas still need review.

The legacy HTTP fixture harness exercises handler contracts below the new access
boundary. Separate authorization tests call the real `Server.ServeHTTP` boundary,
including anonymous and cross-user requests. There is no deployment option to
restore anonymous API access.

## Deployment and cutover

The primary entrypoint is `bash install.sh`, with a first-run wizard and
`update`/`check` commands. `install-go.sh` remains a compatible alias.
See [installer.md](installer.md) for dependency checks, nginx/TLS, progress
and logging levels. The legacy Python entrypoint is `install-python.sh`.

Use Bash, not `sh`: the installers use Bash arrays and `pipefail`.
For a fresh installation with Docker Compose and OpenSSL installed:

```sh
bash install-go.sh --public-url https://aa.ytc-srv.org --aaid-url https://aaid.ytc-srv.org
```

This generates `.env.go` with independent database credentials and a panel
password. Existing environment files, keys and imported panel passwords are
preserved. Default bindings are localhost only: site 18080, AA ID 18091, panel
18092. Configure the host TLS reverse proxy for the public URLs; never expose
the panel port publicly. PostgreSQL has no published port. Keep `.env.go` and
the PostgreSQL/key volumes in protected backups. An existing database with
missing keys fails instead of generating replacement keys.

Public defaults are `https://aa.ytc-srv.org` and
`https://aaid.ytc-srv.org`; the host proxy example is
[`nginx/go-hosts.conf.example`](../nginx/go-hosts.conf.example).
Go validates its own public URLs: only `*.ytc-srv.org` and loopback origins
are accepted. Update removed aliases in an existing `.env.go` explicitly;
the installer preserves credentials instead of silently rewriting the file.
External MIREA authentication, Pulse and schedule hosts remain necessary.

To migrate existing data, first prepare credentials without starting services:

```sh
bash install.sh install --no-start --non-interactive --no-host-nginx --public-url https://aa.ytc-srv.org --aaid-url https://aaid.ytc-srv.org
```

Stop writes to both old services and back up their SQLite databases, master key
and raw 32-byte AA ID signing key. Export those consistent, stopped databases
and import into a separate empty target:

```sh
python3 scripts/export_sqlite_for_go.py --sessions /backup/sessions.sqlite3 --aaid /backup/aaid.sqlite3 --output /backup/snapshot.jsonl
bash migrate-go.sh --snapshot /backup/snapshot.jsonl --master-key /backup/master.key --signing-key /backup/signing.key
```

The snapshot contains credentials: protect it like the databases. The importer
checks decryption, checksums, row counts and empty-target conditions and commits
atomically. Do not run fresh-install startup before importing: it creates panel
configuration and the importer correctly refuses a nonempty target.
The migration script does not stop old services or change the host proxy.
Check health, sign-in, selected-group sync, shared passes and panel access on the
isolated ports before switching the proxy. Keep the old deployment and backups.
To roll back before new writes, stop Go and restore the old proxy; after new
writes, reconcile them first because there is no reverse PostgreSQL exporter.
Never run both writable deployments against diverging snapshots.

Passing tests do not prove the absence of bugs or production readiness. Chromium
tests use synthetic pages, not a real MIREA login. No extended RSS soak test has
been completed. Browser shutdown/response retention fixes are described separately
in [browser-memory-fix.md](browser-memory-fix.md).

## Native entrypoints

`cmd/aa-server` uses `DATABASE_URL`, preserved `MASTER_KEY_FILE`, `AAID_JWKS_URL`
and `AAID_PUBLIC_URL`; `PUBLIC_URL` configures the site callback. Its default
listen address is `127.0.0.1:8082`.

`cmd/aa-id` uses `DATABASE_URL` and `SIGNING_KEY_FILE`, pointing to the preserved
raw 32-byte `signing.key`. Set `SESSIONS_URL` to the native main service and
`AA_SITE_URL` for the callback. Its default address is `127.0.0.1:8093`. Existing
AA ID TTL, session-limit and browser-slot variables are supported. Matching
Playwright driver/Chromium must be installed separately; startup never silently
downloads a browser or replaces a key.

Both accept `-listen`, `-static` and explicit `-init-schema`; `-experimental` is
accepted as a deprecated compatibility flag. Compose uses the owner-only
`aa-init` command for schema, key, role and first-panel-password initialization.

`cmd/aa-import -input <snapshot>` uses `DATABASE_URL` and `MASTER_KEY_FILE` and prints imported counts.
The exporter accepts `--sessions <sqlite> --aaid <sqlite> --output <snapshot>`.
Its output contains credentials and requires the same protection as the source.
Consistency is per SQLite database, not a shared cross-service instant; final
cutover requires coordinated stopped writes and backups.

## Local verification

The completed container/import rehearsal and verification limits are recorded
in [go-verification.md](go-verification.md). The detailed API contract is in
[API.md](API.md).

`go test -race -count=1 ./...` and `go vet ./...` pass with an isolated PostgreSQL
18 instance and Chromium. Database tests use:

- `TEST_DATABASE_URL`
- `TEST_HTTP_DATABASE_URL`
- `TEST_IDENTITY_DATABASE_URL`
- `TEST_IMPORT_DATABASE_URL`

Database names must start with `aa_test_`; import tests additionally require
`_import` at the end. Database tests skip without their variables. Chromium tests
require `TEST_BROWSER_DRIVER` and `PLAYWRIGHT_BROWSERS_PATH`; test documents are
fulfilled in-process without contacting MIREA.

Cross-language fixtures cover crypto, protobuf, 32 HTTP cases, a 15-step sharing
conversation, calendar recurrence and class matching. Browser checks cover
response disposal, cancellation, queue bounds, atomic claims, cookie capture,
consent/optional-action controls and context cleanup. Access tests cover missing
and invalid authentication, foreign accounts, shared-pass scope/revocation,
cross-identity import and write quotas.

Regenerate fixtures only after an intentional contract change:

```sh
python scripts/export_go_contracts.py
python scripts/export_http_contracts.py
python scripts/export_settings_contract.py
python scripts/export_calendar_contract.py
go run ./cmd/aa-contracts -output docs/go-route-coverage.json
```
