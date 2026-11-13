# Performance update

## 2.4.2 — test the nativeApp login parameter

Both production browser runners (AA ID QR login and sessions login/renewal)
now add nativeApp=true to the Pulse login entry URL. rememberMe=True and the
existing HTTPS redirect are retained. Compose forwards MIREA_NATIVE_APP_AUTH
to both services, default 1; set it to 0 in .env.go and recreate the containers
to compare the previous mode. Changing this flag does not rewrite saved tickets.

A public unauthenticated request on 2026-11-13 returned HTTP 302 to the HTTPS
SSO OpenID Connect authorization endpoint with a request_uri. The redirect was
not followed and no account was used. Chromium tests with an offline fixture
verify transmission of the nativeApp/rememberMe query parameters. This does
not verify completion of a real native login, its eventual callback scheme
or any difference in session lifetime. Existing HTTPS navigation guards remain
enforced. A fresh real QR sign-in and observation beyond 24 hours are needed
to test the provider's actual behavior. Production was not accessed.

## 2.4.1 — restore proactive SSO renewal

The 2.4.0 optimization incorrectly cancelled every SSO renewal while GetMeInfo
was valid. A valid profile does not prove that an upstream SSO/refresh session
was renewed, and a provider need not issue a sliding cookie on every request.
Proactive renewal is restored. Its separate encrypted sso_renewed_at timestamp
is set on login/browser renewal and is not moved by ordinary Set-Cookie updates.
Logout removes it. Legacy credentials fall back to their prior renewal time.
Runtime download blocking, cookie persistence, CAS guards and bounded QR-required
handling stay enabled. The 48-hour model now verifies proactive SSO even when
the profile stays valid and cookies rotate. This fixes a confirmed scheduling
defect; the cause of the reported production-wide daily logout still requires
the installed version and renewal status. Production was not accessed.

## 2.4.0 — preserve native-style sliding sessions and fill missing names

The supplied `ru.mirea.pulse-173.apk` has SHA-256
`61b89c1833279e4f0e12b0be8dfaf3b0199b67887ae353f5d5349921736b1ecc`.
Static DEX inspection shows a WebView loading `https://pulse.mirea.ru/pwa`.
`m6.c.onPageFinished` saves CookieManager's Cookie header to `cookie_key` in
SharedPreferences and flushes the cookie store; `G5.c.k` restores the saved
header when creating the WebView. `d6.i`'s stored access token and expiry belong
to DigitalPassRepository, not evidence of an eternal general login. The APK
alone does not establish the identity provider's absolute/idle session limits.

The public PWA inspected on 2026-11-11 requests GetMeInfo with rememberMe=true.
Its [generated profile codec](https://pulse.mirea.ru/assets/shared-D8skLCdC.js)
identifies the authenticated user's first/last names as fields 2/3 and middle
name as a StringValue in field 4. No credentials or private profiles were used
for this inspection. Only the HTML, small entry bundle and first 4 MiB of the
shared bundle were fetched; no framework/runtime download was needed.

Confirmed server defect: ordinary Pulse RPC responses contained headers, but
the application discarded Set-Cookie instead of persisting ticket rotation.
Now every account RPC uses one wrapper that saves cookie updates into encrypted
PostgreSQL credentials and Playwright state. CAS against both sent cookie and
session revision rejects out-of-order responses, logout and fresh-login races.
Chunk replacement, expiry, deletion and cookie-domain/path boundaries are
handled explicitly. Upstream cookies never become client Set-Cookie headers.

The production profile probe now supplies its verified identity, name and
cookie updates in one request. A confirmed live ticket suppresses unnecessary
timer-driven SSO navigation; an actual authentication rejection retains bounded
SSO fallback and requires-login handling. The existing background profile check
is reused, so this adds no periodic network requests and no downloads of the PWA.
Private renewal statistics show cookie_refreshes/last_cookie_refresh separately
from SSO attempts. These are observed cookie updates, not proof of infinite TTL.

Missing display labels are filled from the authenticated profile, including
existing accounts on their next successful refresh. Custom labels are preserved
under a row lock; identity, groups and visibility rules are unchanged. New login
capture and the internal AA ID handoff also use the profile's verified name.
No database schema or client API migration is needed for these two changes.

Local tests simulate 48 hours of short-lived, rotating tickets and verify that
the server retains each new ticket without opening SSO. This proves handling
of that contract, not the real provider's lifetime policy. Production was not
accessed and no live MIREA login was performed.

## 2.2.2 — measuring whether renewal helps

Private panel → System → renewal effectiveness shows lifetime per-account
attempts, persisted successes, failures, QR requirements, interruptions and
last attempt/success times. Confirmed benefit counts a successful attempt once
if it recovered an expired session or increased a known cookie expiry. Unknown
deadlines are reported separately, not presented as proven extensions. Zero
confirmed extensions alone does not prove an SSO heartbeat had no effect.

The last 20 completed attempts retain before/after deadlines, signed expiry
delta and duration. History is fetched for the selected account only. Statistics
are encrypted with the account, survive restarts and fresh QR login, and are
removed with the account. Ordinary logins never become auto-renewal successes.
Observation starts with the new version; previous attempts are not reconstructed.
Opening the dashboard does not initiate new Pulse or SSO requests. If an account
is already paused for a new QR, it remains paused until a new login; viewing its
statistics does not reset that protection or silently restart those attempts.

## 2.2.1 — authentication downloads and broken resource graphs

The reported Grafana burst shows incoming traffic on the physical interface
and forwarding to a Docker bridge. `Mb/s` is megabits, not megabytes. The
image identifies a bridge, not a container or upstream; network names and
container counters are still needed to attribute that production burst.

Confirmed code problems fixed:

- `debug/system` omitted `browser.jobs`, but the panel called its `.map()`
  before requesting metrics. The API now includes a safe bounded job list;
  runtime and resource requests refresh independently and tolerate old replies.
  The previous synthetic UI fixture included that missing field and masked the
  bug. A new Chromium test renders the actual Go API with PostgreSQL auth.
- Charts render the current snapshot immediately, including one point, instead
  of waiting for several minute samples. Snapshot errors are distinguished.
- Normal login/renewal blocks Pulse application scripts, styles, framework and
  content bundles, WASM/DLL/data files, fonts and media before transfer. SSO and
  anti-bot challenge resources remain enabled; direct RPCs verify identity/group.
  Service workers are blocked in auth contexts so they cannot bypass routing.
  Only explicit administrator recording may load the full application UI.
- A static cache did not prevent repeated downloads of non-cacheable assets or
  runtimes exceeding its budget. Auth now avoids loading that application.
- The early-renewal margin is capped at 20% of the observed cookie lifetime:
  a 15-minute margin cannot immediately renew a fresh 10-minute cookie. The
  existing periodic SSO heartbeat is retained for both known and unknown expiry:
  SSO idle lifetime can be shorter than the Pulse cookie's visible deadline.
  Traffic is reduced by excluding the application runtime, without assuming a
  longer upstream SSO idle timeout.
- An interactive SSO login page stops silent renewal promptly. The encrypted
  `requires_login` result survives restarts and pauses futile attempts until new
  credentials are saved. Stale jobs cannot pause a newly replaced session.
  Existing Pulse access remains valid until it actually expires. Both services
  now request persistent Pulse cookies with `rememberMe=True` on new login.
- Stored cookies were not proof of successful renewal. The private panel shows
  running/failed/requires-login outcomes and the last attempt and login/success.
  It also shows safe per-host/type browser byte and blocked-resource counters.
- Metrics upgrades used to probe with `--once` while the daemon held the state
  lock. The installer now stops it before probing, preserves counters, and
  restores the prior collector code/service if the probe fails.

SSO may still require another QR: this change cannot override MIREA's absolute
session lifetime, revocation or reauthentication policy. Local tests do not
establish those upstream lifetimes or prove a real MIREA login; production was
not accessed. The earlier implementation notes below are historical.

The deployment sample showed 218 GB received by the sessions container over five
days, with little traffic received by AA ID or Nginx. This points investigation
toward outbound work in sessions; it does not prove which upstream produced it.
Local HTTPS health checks took 15–17 ms. External checks were fast on a reused
connection and slow on a new connection. The changes below do not claim to fix
that separate first-connection delay.

## Changes

- Browser renewals revisit Pulse every 15 minutes per account. Playwright routing
  disables its normal HTTP cache. A process-wide LRU cache now reuses Pulse JS,
  CSS and fonts only when the upstream explicitly sends `public, max-age=...`.
  API calls, SSO and protection challenges bypass it. Responses with cookies,
  private/no-cache/no-store directives or personalized Vary headers are excluded.
  Limits: 32 MiB total, 4 MiB per asset, 128 entries and 15 minutes lifetime.
- `/panel/api/debug/system` includes `browser.assets`: hits, misses, stored bytes,
  downloaded bytes for eligible assets and saved decoded bytes. These are not
  total container wire traffic; use Docker counters to measure the total impact.
- Static files may be stored and revalidated with ETags, allowing 304 responses.
  APIs and HTML retain their existing no-store policy.
- Expired saved calendars are served while one shared background task refreshes
  each group. The first request without a saved calendar still waits upstream.

## Install on an existing deployment

Copy the updated source while preserving `.env`, Docker volumes and host Nginx
configuration. Rebuild the two application services from the project directory:

```sh
sudo docker compose up -d --build --no-deps sessions aaid
sudo docker compose ps
```

This restarts the application containers; perform it outside active QR sign-ins.
Do not run the installer or remove volumes for this update.

Compare Docker network counters over equal time windows with similar usage,
and check `browser.assets` in the admin system diagnostics. If Pulse does not
mark its assets public/cacheable, they continue to download normally and cache
hits will remain zero. Live QR and renewal behavior still require deployment
verification; automated tests use simulated upstream responses.
