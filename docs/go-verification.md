# Current verification note: 2.4.2

The nativeApp=true/false URL construction preserves the HTTPS destination,
redirect and rememberMe=True. An offline Chromium navigation test verifies
the actual outgoing query. Browser tests and builds of both services pass.
A public unauthenticated nativeApp login request returned 302 to HTTPS SSO;
no redirect, live sign-in or production access was performed. Real callback
compatibility and longer session lifetime remain unverified.

## Previous verification: 2.4.1

The production profile adapter no longer suppresses proactive SSO renewal.
The 48-hour model now asserts that a valid profile and rotating cookies do not
postpone the separate SSO heartbeat; authentication rejection still respects
retry backoff and requires-login handling. HTTP/PostgreSQL race suites pass;
other packages passed during the full race run. Both Linux services compile.
No production connection or live university authentication was performed.
The reported simultaneous website logout still requires checking the deployed
version and AA ID session state; its validity is independent of Pulse state.

## Previous verification: 2.4.0

The full Go race suite passed with four isolated local PostgreSQL databases and
real Chromium enabled. A bounded fake university transport models 48 hours of
one-hour sliding tickets: every rotated cookie is stored and used on the next
probe, no SSO browser is opened for a valid profile, and a real authentication
rejection invokes the existing bounded SSO fallback. Name extraction uses the
authenticated UID and the official PWA's protobuf field layout. PostgreSQL
tests cover preserving custom labels and pass numbers, cookie counters and
stale responses after logout; a concurrent logout cannot fill a name or revive
the account. Pass enrollment is tested with cookie rotation between token and
issuance, without exposing Set-Cookie to the client.

Chromium verifies restoration of rotated Pulse cookies while preserving SSO
state. Existing website, pass, metrics and QR tests also passed. Focused race
checks were repeated after final cookie attribute/expiry handling adjustments.
Both Linux service binaries compile with Go 1.26.10; JavaScript syntax and API
reference checks pass. The APK/PWA findings and their limits are recorded in
performance-update.md. Production was not accessed; no live MIREA login or
real-world multi-day lifetime guarantee was tested.

## Previous verification: 2.3.0

Digital passes use server-owned JSON operations shared by the website and app.
The full Go race suite passed against four isolated local PostgreSQL databases;
after the final pass-switch changes the HTTP, PostgreSQL and Pulse race suites
passed again, including real Chromium with the actual Go API and PostgreSQL.
The university transport in those tests is synthetic. Cases cover anonymous
access, explicit shares, revocation, disabled passes including the owner,
foreign groups, wrong codes, malformed responses, rate limits, concurrent
operations, logout/disable during enrollment and int64 precision. The browser
test exercises code request, enrollment, number retrieval, clearing displayed
credentials and absence of local/session storage writes. Existing client RPC
and number publication endpoints stay retired.

Both Linux service binaries compile with Go 1.26.10. JavaScript syntax and the
API documentation checker pass (92 retained legacy routes, 13 additions and
48 JSON examples). Existing app builds need the JSON integration described in
client-api-migration.md; the Flutter application was not modified here.
Production was not accessed. Live university pass issuance, remote device
binding/revocation and actual NFC gate acceptance were not exercised.

## Previous verification: 2.2.2

Renewal observations persist inside the encrypted account record. PostgreSQL
tests verify automatic success, recovered expiry, known/unknown/unchanged cookie
deadlines, no double-counting, manual login exclusion, reopening the vault,
history capped at 20 and rejection of stale callbacks. Chromium with the real
API verifies the summary counters and selecting an account's deadline history.
The site's CSP remains strict; the browser test uses normal locators/actions.
Statistics retrieval reads saved data and does not trigger upstream renewal.

## Previous verification: 2.2.1

The metrics regression is covered with a real Go HTTP server, PostgreSQL panel
login and real Chromium, including a collector with no minute history and a
runtime-diagnostics failure. A separate compatibility fixture omits jobs, as
older Go responses did. Chromium also verifies Pulse runtime requests are
blocked before transfer and an interactive SSO page is detected promptly.
Renewal tests cover known/unknown deadlines, short cookie lifetimes, durable
requires-login state, fresh-login reset and rejection of stale renewal writes.
Collector installer tests cover stop-before-probe, preservation of counters and
rollback of the prior code/service on probe failure. See performance-update.md
for the distinction between confirmed code defects and production attribution.

## Previous verification: 2.2.0

The local security regression suite covers live identity revocation and current
group checks, whole-day foreign names with per-class attendance masking,
conservative shared-class matching, inclusive ±30 minute lesson selection,
early marks, QR result expiration/cancellation and bounded HTTP/calendar work.
Host resource monitoring includes every process in the Compose service cgroups;
counter persistence is tested across restarts, container recreation and resets.
See [security-audit.md](security-audit.md) and [API.md](API.md).
Production was not accessed; external MIREA authentication was not exercised.

2.2.0 checks: full `go test -race ./...` against four isolated local PostgreSQL
databases passed, followed by focused race checks after the final changes.
Installer/nginx checks: 23 tests and 16 subtests passed; Python migration and
counter persistence checks: 12 tests and 3 subtests passed. Linux collector
checks passed (4 tests), including real Unix-socket HTTP against a fake daemon.
Real Chromium passed lesson navigation and metrics rendering, QR/consent/cookie
capture, repeated context cleanup and browser recycling, entirely offline.
Linux sessions/AA ID binaries compile; JS and Bash syntax checks and the API
reference checker pass (92 retained legacy routes, 8 additions, 44 JSON examples).
The builder is pinned to Go 1.26.10. After updating x/crypto to
v0.58.1-0.20261009181718-2f1f834330b2, the full PostgreSQL race suite passed
again on Go 1.26.10; govulncheck v1.9.0 on that toolchain reports no vulnerabilities.

The application now uses website operations. Client-authored pass numbers,
raw Pulse RPCs, cookie imports and daily pass testing were retired deliberately.
Server-owned QR login has a native token response, while protected AA ID web
operations and main site operations accept either their web session or an app
Bearer. See [client-api-migration.md](client-api-migration.md).

Current route coverage is 92/92 retained legacy routes (8 exclusions) plus
8 native additions. The API reference validates 44 JSON examples. Historical
parity counts below describe the original migration, not compatibility with
retired endpoints. Local PostgreSQL integration tests cover website/app result
parity, class-scope authorization, one-shot QR claiming, token refresh/revocation,
no Pulse credentials in app grants, and rejection of retired workflows.

# Local Go migration verification

Verified on 2026-11-06. No production host or production database was accessed.
All account credentials used in tests are synthetic.

## Implementation coverage

- 97/97 in-scope source HTTP routes have Go handlers; zero remain unported in
  the route inventory. Three Telegram routes are intentionally excluded.
- Native sessions and AA ID executables use PostgreSQL. SQLite is read only
  by the standalone export tool, outside the Go runtime.
- Native deployment, fresh installation, preserved-key initialization and
  transactional import tooling are provided separately from the legacy stack.
- The API reference covers those routes, two additional native diagnostics,
  authorization, parameters, errors and 44 checked JSON examples.

Route registration coverage is not proof of identical behavior for every
possible upstream response. The following checks provide the actual evidence.

## Checks completed

| Check | Result |
| --- | --- |
| `go test -race -count=1 ./...` | Passed, with all four PostgreSQL test database variables and Chromium enabled |
| `go vet ./...` | Passed |
| Legacy Python export/browser/renewal/cache/nginx tests | 37 passed |
| Legacy AA ID browser memory tests | 4 passed |
| Bash syntax for installation/migration scripts | Passed |
| `scripts/check_api_docs.py` | 97 source routes + 2 native additions, 44 valid JSON examples |
| Native Linux image build | Passed; Go binaries compiled inside Linux builder |
| Fresh Compose stack | PostgreSQL, sessions, AA ID and nginx healthy; initialize completed successfully |
| Imported-data Compose stack | Healthy after import, restart and image recreation |
| `scripts/verify_go_stack.py` | 33 HTTP checks passed against each stack |
| Runtime database roles | sessions can access aa only; aaid can access aaid only |
| Runtime key mounts | Each non-root application sees only its own key |
| Chromium in the runtime container | about:blank rendered under uid 10001 with read-only root filesystem |
| Public domain policy | Foreign origins rejected by both Go entrypoints before key/DB access; installer rejects before credential generation |
| Host nginx template | `nginx -t` passed in an isolated container |

The fresh and imported stacks use separate local ports, Compose projects and
volumes. Repeated initialization preserved existing keys and configuration.
The migrated fixtures were created by the actual Python Vault and AA ID Store:
18 tables, 13 rows, two accounts, one identity and an existing refresh session.
The snapshot was exported, imported and decrypted successfully. The preserved
refresh token was accepted by Go, rotated, and used to obtain an access token.
The token selected only its owner's accounts/events; foreign account reads/RPCs
were refused. The old signing seed produced the same public JWKS key after
restart. No real Pulse cookie was required for this rehearsal.

Cross-language fixtures additionally cover cryptography, protobuf, 32 HTTP cases,
a 15-step sharing conversation, calendar recurrence/class matching and journal
decoding. Dedicated access tests exercise the real network authorization
boundary, including missing/invalid tokens, foreign imports, shared-pass scope,
group filtering, revoked access notifications and quotas.

Browser tests cover disposal, cancellation, expiry cleanup, bounded queues,
atomic claims, cache coalescing, larger framework assets, conditional validation
and repeated creation/closure of actual Chromium contexts. These tests establish
resource lifecycle behavior, not a multi-day resident-memory measurement.

## Limits of this verification

- Live MIREA QR authentication, silent renewal, attendance and digital-pass
  issuance have not been exercised with a real university account.
- No production load test, multi-day RSS soak or production cutover was run.
- The source exposed concrete repeated-download/retention mechanisms; local
  tests verify the fixes. Historical attribution of all 200 GB requires actual
  deployment traffic measurements and is not established by these tests.
- Main-service JWT validation is offline: a revoked AA ID device can retain
  access until its short-lived access token expires. Quotas are per process.

Deployment/backup/import/rollback instructions are in
[go-postgres-migration.md](go-postgres-migration.md). The full Russian API
reference is [API.md](API.md). The original Python deployment remains available
for rollback; no automatic production replacement was performed.
