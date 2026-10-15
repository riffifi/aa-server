# Pulse session server — URL-only automatic sync

Enter the same server URL in **Settings → Session server** on each phone. There
are no device keys, pairing steps, publisher switches, or manual upload steps.
The app uploads its own saved sessions and NFC pass identifiers automatically.
Other connected apps receive revision events and fetch the changes. A new login
or NFC enrollment schedules an upload immediately; failed uploads retry while
the app runs and when it resumes. Unchanged credentials are not uploaded again.

Pulse cookies stay encrypted on the server and are never downloaded to phones.
Phones proxy the supported Pulse profile, attendance and NFC RPCs through the
server. Each NFC activation requests a fresh account-bound Pulse token. The
server cannot inspect the official Pulse app’s private storage on a phone.

**Anyone who can reach the API can read shared account metadata, use passes
whose use is enabled, and upload sessions and permitted passes.** The URL is not an authentication secret. Use
network/firewall/VPN rules or Nginx access rules to restrict access if needed.
The administration panel still requires its own password and CSRF token for
account removal and login management. HTTPS encrypts traffic; it does not add
phone authentication. Plain HTTP is supported.

## Self-service account picker

Opening the server URL now shows a **self-service picker** instead of the admin
sign-in: account owners see every account with its connection state, tap their own
to renew it (confirming the normal MIREA sign-in), or **Add my account** to enroll
for the first time. No panel password is needed — this matches the URL-is-the-
capability model, so restrict access with your network/Nginx if required. The
administration panel (account removal and login management) moved to
`PUBLIC_URL/panel` and still requires its password and CSRF token.

`POST /api/accounts/import`, `GET /api/accounts` and the new unauthenticated
`POST /api/logins` (`{label, account_id?}` → a temporary `/enroll/{id}/{token}`
path the picker polls) back this flow. Destructive panel endpoints are unchanged.

The QR login now presses through Pulse's post-approval "continue"/consent page
automatically, so a sign-in no longer stalls at *confirming the session*; the same
step runs during silent renewal.

### Per-account page (password-protected)

Every account has its own page at `/u/{account_id}` showing the session status, a Renew
button and the pass permission. The page is **protected by a per-account password**: on
the first visit the owner sets one, and after that it must be entered (or a session
obtained by signing in via the university — see below). The password is scrypt-hashed in
the account's encrypted secret; a `page_session` cookie (30 days) unlocks the page and
gates the account-detail read and the pass toggle. Endpoints: `GET /…/page` (status),
`POST /…/page/password` (set or change — **always requires an active session**, so a
passwordless account cannot be hijacked by whoever sets a password first; the first
session comes from signing in via the university),
`POST /…/page/login`, `POST /…/page/logout`. Manual group entry is **not** on this page —
it lives only in the admin panel.

### Sign in via the university (dedicated page)

The home page keeps the account list and adds a **"Войти через университет"** button
that links to a dedicated page at **`/login`**. That page runs the normal MIREA QR
login and, on completion, `POST /enroll/{id}/{token}/claim` issues a page session for
the account the login produced and sends the user to `/u/{account_id}`. Because the
login also captures and stores the session, signing in this way renews it at the same
time. The sign-in sends **no label**, so it never overwrites an existing account's name.

### Ending a session

`POST /api/accounts/{id}/end-session` (owner page session, or the `/panel/...` variant for
admins) terminates the Pulse session without deleting the account: it best-effort calls
MIREA's logout with the stored cookie, then clears the cookie and browser state from the
encrypted secret, marks the account `expired`, and emits a sync event. The label, group,
page password and pass permission are kept; auto-renewal won't run until the owner signs in
again. Endpoints that need a live session (RPC proxy, attendance, profile dump) return 409
afterwards. The owner's page has a "Завершить сессию" button and the panel a per-account
action. See [docs/migration-plan.md](docs/migration-plan.md) for the plan to move the rest
of the client's logic server-side for security.

### Renaming

An account is renamed via `PUT /api/accounts/{id}/label` (`{label, expected_version?}`) —
allowed only from the admin panel (the `/panel/...` variant, password + CSRF) or from the
owner's own page (a valid page session). Renames bump the version and emit a `label` sync
event. The owner's page and the panel both expose it. Page passwords are set and changed
from the owner's page (always under a session).

### Pass permission

Each account carries a `pass_allowed` flag (on by default) that the owner can toggle
from their page, the picker, or the bot. When it is off the server refuses the
`pass-token`, `pass-code` and `pass-issue` RPCs for that account with HTTP 403, so the
app cannot use or issue that account's digital pass; the session, attendance and
renewal are unaffected. `GET /api/accounts/{id}` returns one account (including
`pass_allowed`, which also appears in `GET /api/accounts`), and
`PUT /api/accounts/{id}/pass-allowed` (`{allowed, expected_version?}`, public; the
`/panel/...` variant needs the password) changes it and emits a `pass_permission`
sync event so phones pick it up.

## Telegram bot

Set `TELEGRAM_TOKEN` in `.env` (empty by default) and rerun `sudo ./install.sh`
to enable an optional bot. With the variable unset the server runs exactly as
before. The bot uses long polling (no public webhook or HTTPS needed) and drives
the same in-process login and renewal as the panel and picker — it adds no new
stored secret beyond the chat↔account link used to address alerts.

In the bot an owner sends `/menu`, picks their account (which turns on alerts),
and renews or enrolls through inline buttons that show the MIREA QR as a photo.
When an account's session can no longer be renewed automatically, everyone who
linked that account gets a **"needs a new login"** message with a one-tap Renew
button. When `PUBLIC_URL` is HTTPS the menu also offers a mini-app button that
opens the picker inside Telegram; on an HTTP deployment it links out to the
picker in a browser instead.

Rotate the token with BotFather's `/revoke` if it has ever been shared, since the
token alone controls the bot. Never commit `.env`.

## Concurrency and self-service load

Interactive logins and background renewals use **separate browser budgets**, so a
burst of people enrolling through the picker can no longer be starved by renewals
(or by each other). An interactive login holds its browser slot for the whole QR
wait, so the pool — not a single shared pair of slots — sets how many people can
enroll at once. Defaults, all env-tunable: `LOGIN_SLOTS=6` interactive logins,
`RENEW_SLOTS=2` background renewals, `MAX_ACTIVE_LOGINS=24` before new starts are
refused with a "server is busy" message, and `LOGIN_TTL_SECONDS=600` before an
unfinished login releases its slot. Each slot is one headless browser context, so
on a small host lower `LOGIN_SLOTS` (and raise `shm_size` in `compose.yaml` if you
raise it); on a larger host raise it to let more of a class enroll simultaneously.

## Automatic renewal

The server keeps each active SSO state warm and rolls it forward without any user
interaction: it renews when a known expiry is within `RENEW_BEFORE_SECONDS` (default
900), re-exercises an active session at least every `RENEW_INTERVAL_SECONDS`
(default 900), scans every `REFRESH_SECONDS` (default 180), and backs a single
account off for `RENEW_BACKOFF_SECONDS` (default 300) after a failed attempt. A
failed proactive renewal keeps the still-valid credentials. The server cannot make
a session permanent once MIREA's own SSO state requires a fresh login — that is
when the Telegram alert and the picker's **Needs re-login** badge come in.

## Group discovery (diagnostic)

To work out where a student's group lives in the Pulse profile (for future automatic
group assignment), an admin can call `GET /panel/api/accounts/{id}/profile-fields`
while signed in to the panel. It fetches the account's profile RPC and returns the
decoded protobuf as a field tree (`{grpc_status, fields}`) — strings, numbers and
nested messages only; cookies and tokens are never included. Use it to locate the
group name and numeric schedule ID, then wire automatic assignment.

## Attendance (turnstile entry/exit)

`GET /api/accounts/{id}/attendance` (owner page session, or the `/panel/...` variant for
admins) returns today's turnstile events for the account, via Pulse's
`HumanPassService/GetHumanAcsTimeline` (request: the account's pulse id + a Moscow-day
window + flags 3=1, 4=2). Events are collected recursively (an event is any submessage
with a Timestamp in field 2, a zone in field 3 and a direction code in field 5). The
direction is the **code in field 5**, verified against real passes: at the inner turnstile
`1` is an entry and `2` is an exit; the outer checkpoint (ЦКПП) emits `3` in both
directions and is ignored. The response reports `{inside, events:[{direction, time, zone}],
since}`, `inside` = last directional event is an entry. The owner's page shows "Посещение
сегодня" with an inside/outside badge and the entry/exit list; the panel has a per-account
"Посещение" action.

## Automatic group detection

The group is detected from the account itself: Pulse's
`rtu.schedule.api.PersonalScheduleService/GetSubscribedSchedules` (empty request,
identity from the cookie) returns the subscribed group with its numeric schedule id
and name — the same id space as `schedule-of.mirea.ru`. `refresh_account` calls it and
assigns the group the first time an account is seen valid, without overwriting a group
set by hand, so new logins get their group automatically within one refresh cycle.

## Group lookup (manual fallback)

When detection finds nothing, `GET /api/groups/search?q=<name>`
resolves a group name to its numeric schedule id through the public MIREA schedule API
(`schedule-of.mirea.ru/schedule/api/search`, `scheduleTarget == 1`), the same source and
id space the official app uses. The per-user page has a live search box that calls it and
saves the chosen group through the existing `PUT /api/accounts/{id}/group`. The server
host must be able to reach `schedule-of.mirea.ru`.

## Groups

Assign university schedule group IDs and names from an account’s **Group** action
in the panel, or **Assign group** in the app. Group edits sync without a new login
and preserve session and pass revisions. The panel checks the account revision
before saving; refresh and retry if another client changed it.

In **Settings → My group**, choose a group to fetch only its accounts and passes.
**All groups** clears the preference. The Attendance selector filters the accounts
already available on the phone and scopes selection to that group. Newly added
local accounts without a group inherit the phone’s preferred group. Existing
local accounts from other groups remain stored when hidden.

`GET /api/groups` lists group IDs, names and account counts. Account snapshots and
long-poll events accept `group_id`; moving an account notifies both its old and new
groups. `PUT /api/accounts/{id}/group` accepts `group_id`, `group_name` and
`expected_version`. A null group removes an assignment. Filtering is a sync
preference, not an API access restriction.

Deploy the updated server before using group-only fetches in the updated app.
Rerun `sudo ./install.sh` after copying/pulling these changes; existing encrypted
accounts are preserved, and event columns migrate automatically at startup.

## Docker + Nginx installation

Clone or copy this separate server repository, then run inside its directory:

```sh
sudo ./install.sh
```

The default public URL is **http://aa.yellowteacrew.ru**. Point its DNS record at
94.29.36.13 and allow incoming port 80. The installer installs Docker, Compose,
Nginx and Python 3 when needed on Ubuntu/Debian. Docker's proxy listens only on
127.0.0.1:8080; the host Nginx serves the domain on port 80.
The Python service has no published host port. Its image installs only the
[headless Chromium runtime](https://playwright.dev/python/docs/browsers#chromium-headless-shell)
used for MIREA login, rather than downloading all browser engines.

Nginx forwards the panel, HTTP gRPC-Web requests, and the long-poll update feed.
Caching and buffering are disabled, and its upstream read timeout exceeds the
25-second event wait. Access logging is disabled so enrollment capabilities do
not enter access logs. Configuration: `nginx/default.conf`, based on the
[official Nginx proxy module documentation](https://nginx.org/en/docs/http/ngx_http_proxy_module.html).

Open the configured URL in a browser to manage accounts. To read the generated
**panel password**, run inside the server directory:

```sh
sudo awk -F= '$1 == "ADMIN_PASSWORD" {print $2}' .env
```

The password is independent of the SSH password. Existing panel credentials
and the encrypted Docker data volume are preserved on reruns. Phone setup only
needs the URL. Check arguments without making changes:

```sh
./install.sh --check
```

## Existing host Nginx / HTTPS

The installer discovers the global configuration using `nginx -T`, creates
`antiattendance-managed/sessions.conf` next to it, and inserts one managed include
inside its `http` block. Existing sites are preserved. Repeat installs do not
append duplicate includes. Global configuration backups are saved next to the
original file. Both configuration validation and reload must succeed; otherwise
the previous configuration and managed site are restored.

For a custom global config use `sudo ./install.sh --nginx-conf /path/nginx.conf`.
An existing unmanaged server block for the same domain is reported as a conflict
rather than overwritten. Host mode updates only PUBLIC_URL, BIND_ADDRESS, PORT
and DOMAIN in `.env`; panel credentials and stored accounts remain intact.

For direct Docker access without host Nginx:

```sh
sudo ./install.sh --no-host-nginx --url http://94.29.36.13:8080 --bind 0.0.0.0
```

HTTP is configured by default; no certificate is issued. If you add HTTPS to the
managed site, set PUBLIC_URL to your HTTPS origin in `.env` and use that address
in the app. Rerunning host setup regenerates the managed HTTP site, so maintain
custom TLS configuration separately and use `--no-host-nginx` for subsequent
application upgrades.

## Upgrade from the device-key server

Update this server checkout and the app together. Protocol version 2 is URL-only;
old device headers are ignored and device-management endpoints are removed.
Existing account cookies, saved browser state, passes and panel password remain.
On phones the saved URL is retained and the obsolete key is removed. The sync
journal is stored separately for each server origin, so changing servers does
not reuse another server’s revisions.

```sh
# From this server repository; keep the existing .env and Docker data volume.
sudo ./install.sh
```

The script preserves `.env` and removes the obsolete Caddy container with
`--remove-orphans`. If your old URL relied on Caddy HTTPS, configure your host
Nginx for that URL, or change `PUBLIC_URL`/`BIND_ADDRESS`/`PORT` in `.env` to the
new HTTP listener before upgrading. Do not use `docker compose down -v`.

## Synchronization rules and limits

Pulse verifies every uploaded cookie against its read-only profile RPC and
checks account identity before replacing credentials. Session and pass revisions
are independent. The app persists the revision known before an offline edit;
a stale upload receives a conflict and adopts the newer server version instead
of overwriting it. Initial copies of existing accounts adopt the shared session
and pass. Retained local copies do not resurrect an account deleted in the panel.
One failed upload does not prevent other accounts from syncing.

A connected phone’s event feed updates the app immediately. Disconnected or
OS-suspended apps catch up when they resume. Android cannot guarantee delivery
to an app the OS has stopped; this implementation does not use Firebase push.
Local accounts and their passes remain in secure storage for disconnect/offline
recovery; the sync journal contains hashes, revisions, and pending pass numbers.

Account owners can also sign in from the panel’s MIREA QR or a temporary ten-minute
enrollment link. Browser login is confirmed by the read-only Pulse identity RPC.
MFA and forced logout still apply. Panel enrollment saves encrypted MIREA browser state. The server attempts renewal
when a known expiry is within ten minutes, every thirty minutes for active
sessions, and after a confirmed expiry. Failed attempts back off for five minutes;
a failed proactive renewal keeps the still-valid credentials and enrolled pass.
The panel indicates which accounts have saved state for automatic renewal.
Cookie-only phone uploads need one panel re-login to enable this behavior.
The server can renew while MIREA accepts the saved SSO state, but cannot make
sessions permanent. QR approval is polled by one worker and submitted exactly
once; redirect-time profile failures are retried, and approval without a confirmed
Pulse session produces an actionable error after ninety seconds.
HTTP 403, network outages and gateway HTML do not falsely expire sessions.

## Storage and operations

The named `session_data` volume stores SQLite plus `master.key`. Cookies, browser
state and pass identifiers use authenticated Fernet encryption. The panel
password is hashed; its login cookie is HttpOnly/SameSite Strict. Browser working
files live in container tmpfs. Back up the database and key together.

One application process owns login jobs and event notifications. Keep Uvicorn at
one worker and do not run multiple service instances against this SQLite volume.
Profile validation defaults to five minutes (`REFRESH_SECONDS`).

```sh
docker compose logs --tail=100 sessions nginx
docker compose up -d --build --remove-orphans --wait
docker compose stop
```

## Tests

```sh
python3 -m venv .venv
.venv/bin/pip install -r requirements-test.txt
.venv/bin/python -m pytest -q tests
bash -n install.sh
```

Tests use simulated Pulse replies; a real owner-approved sign-in must still be
verified after installation. Never commit `.env`, cookies, database backups,
`master.key`, or login capabilities.

## Daily pass testing

The authenticated administration panel shows a separate **Pass testing** password,
with a copy action and a preview of available test passes. It rotates at midnight
in Moscow and is stable across server restarts. It is not the panel login password.

In the app, open the debug menu, enable **Pass testing**, and paste that password.
The app keeps it in platform secure storage for this server until it expires or
is rejected, then requires the new password. Explicitly disabling testing removes
it. Test-only pass numbers are excluded from the persisted account snapshot, and
NFC token expiry is capped at the testing deadline.

`POST /api/debug/pass-testing` validates the daily password with rate limiting.
Testing API requests use `X-Pass-Test-Key`. This permits pass snapshots, uploads,
and pass RPCs while the saved `pass_allowed` flag is false, without modifying the
flag. Pulse identity, live-session checks, account-bound token issuance, owner
verification-code requirements and attempt limits still apply. Ordinary requests
cannot retrieve disabled pass numbers. Testing unlocks and exceptional pass
operations are recorded in `audit_events` without passwords or tokens; records
are bounded to 30 days and 10,000 entries.
