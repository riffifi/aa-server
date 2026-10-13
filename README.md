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

**Anyone who can reach the API can read and use all shared accounts and passes,
and upload sessions and passes.** The URL is not an authentication secret. Use
network/firewall/VPN rules or Nginx access rules to restrict access if needed.
The administration panel still requires its own password and CSRF token for
account removal and login management. HTTPS encrypts traffic; it does not add
phone authentication. Plain HTTP is supported.

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
