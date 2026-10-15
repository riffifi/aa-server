# Migration baseline · 16 October 2026

Baseline force-synced from `origin/main` at `c9ffbca`. The prior tracked source was
archived before reset. This records Phase 0 of the app's `migration.md`; it does
not claim that the URL-only protocol is authenticated.

## Endpoint access matrix before migration

| Routes | Current verified access |
| --- | --- |
| `/health`, static pages, `/panel`, `/login`, `/u/{id}` | Public shell/health; page data has separate checks. |
| `/panel/api/login` | Panel password; same-origin check; five attempts/minute/address. |
| `/panel/api/state`, login state/QR, account profile-fields | Panel session. |
| Panel logout, login start/cancel, refresh/delete, label/group, pass permission, end-session | Panel session plus CSRF. |
| `/api/logins` | Public login start; enrollment capability protects the resulting job. |
| `/enroll/{id}/{token}` and state/QR/claim | Random hashed, expiring enrollment capability. Claim establishes the owner's page session after university approval. |
| `/api/accounts`, `/api/events` | Public shared metadata/pass snapshot and event feed, optionally group-filtered. **Group filtering is not authorization.** |
| `/api/groups`, `/api/groups/search` | Public schedule-group metadata. |
| `/api/accounts/{id}` | Owner page session. |
| `/api/accounts/{id}/attendance` | Owner page session; panel variant requires panel session. |
| `/api/accounts/{id}/page` | Public minimal identity/authentication status; fuller metadata after owner login. |
| Page password / login / logout | Password change needs active owner session; login checks the hashed password with rate limiting; logout deletes the presented session. |
| Public label / end-session / pass-allowed | Owner page session. |
| Public group mutation | **Unauthenticated**; revision/CAS protects conflicts, not ownership. |
| `/api/accounts/import` | **Unauthenticated**; Pulse identity/account/revision checks remain valuable additional constraints. |
| Public pass mutation | **Unauthenticated**; revision checked. |
| `/api/accounts/{id}/rpc/{method}` | Public allowlisted, size/framing-checked proxy; pass methods additionally check `pass_allowed`, issued token/account binding and attempt limits. |

The account snapshot currently returns stored pass numbers even when pass use is
disabled. The testing feature must not perpetuate this discrepancy: ordinary
snapshots should honor permission, with an explicit, expiring admin-authorized
exception for testing.

## Data-flow inventory

- The Flutter app's local `AccountStore` persists local Pulse cookies in platform
  secure storage. Local sign-in, restoration and Nearby/QR transfer still exist.
- `SessionAutoSync` treats local secure account/pass stores as an upload outbox,
  records observed revisions and retries through a persistent sync journal.
- The server validates imported Pulse identity and encrypts cookies, browser SSO
  state and stored pass identifiers with Fernet under `master.key`.
- Server snapshots omit cookies/secrets. Remote accounts use
  `server-session:<account-id>` references in the app and an HTTP proxy for RPCs.
- Remote `NfcPassStore.number()` reads the active server client's snapshot;
  local pass records bind the number to the local session fingerprint.
- Pass-token, verification-code and issuance RPCs stay bound to the Pulse
  account/token flow. A testing permission exception must not bypass those checks.
- NFC presentation/diagnostics remain phone-side. Passes are sent to Android's
  NFC service only while the pass screen's lifecycle permits it.
- Session termination clears cookie/SSO renewal state, preserves account/group/
  permission metadata and emits a sync event. It needs explicit transport-failure
  and renewal-race regression coverage.
- Public deployment TLS is currently configuration-driven (`PUBLIC_URL`), not
  enforced by the service. SQLite and in-memory jobs/events require one process.

## Decisions and remaining protocol work

The user chose a separate daily admin password for pass testing, copied from the
panel and retained in app secure storage for that server until rejected/expired.
This is distinct from the panel login password. Local Pulse sessions and existing
transfer/recovery flows are retained in a Legacy view under app Settings, while
the normal app view uses server-backed account references and server sign-in.
Legacy synchronization compatibility remains available; the app therefore still
contains reusable local credentials when the user uses Legacy.

A general phone-client authorization model for the URL-only API has not been
chosen by those answers. The testing capability is deliberately scoped to pass
permission and does not grant group, ownership or account-management privileges.
Do not claim Phase 1 authentication is complete or remove owner/panel protections.
PostgreSQL remains deferred. The next protocol step must coordinate normal-client
authorization with existing synchronization and the Legacy compatibility policy.

The separately requested daily-password pass testing mode is an explicit admin
exception to the owner's pass-use flag, not a persistent change to that flag or
a substitute for Pulse sign-in, verification, account binding or token expiry.

## Per-person Telegram pass authorization · 16 October 2026

The owner now approves each Telegram person separately. `X-Pass-Client` is a scoped
app pass credential, linked to the requester's Telegram through a separate,
single-use ten-minute handoff. A verified owner (MIREA sign-in through the bot)
approves/denies the request or revokes the grant. Old chat↔account alert links
are unverified and cannot decide permissions. Pass snapshots, uploads and pass
RPCs enforce the grant plus the global account switch. Normal snapshots omit
unauthorized pass numbers. The daily admin password is the explicit testing
exception; it creates no persistent grant. Metadata/attendance authorization
remains a separate Phase 1 decision. Revocation emits an event; native app
activation has a maximum five-minute online-refreshed lease. Deploy app and
server together. Bot configuration is held in an ignored private environment
file, with no token in source or tests.
