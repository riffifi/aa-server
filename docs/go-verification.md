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
