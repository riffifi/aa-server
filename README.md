# Central Pulse sessions and NFC passes

The server owns the Pulse cookie jar. Phones authenticate with revocable device
keys and proxy attendance/session/pass calls through the server. No API returns
Pulse cookies. An account owner signs in once through an enrollment QR; subsequent
renewal in the panel updates every permitted phone. University MFA and forced
logout still apply: the server can renew from its saved MIREA SSO state while that
state remains valid, but cannot make sessions permanent.

## Install with Docker

Copy this repository onto an Ubuntu/Debian server, then:

```sh
cd server
sudo ./install.sh --url http://192.168.1.20:8080 --bind 0.0.0.0
```

Use the server's actual LAN/VPN address. HTTP is supported for a private network
or VPN. Plain HTTP exposes the panel password, device keys, login challenges and
pass identifiers to anyone able to observe that connection; do not expose this
HTTP listener directly to the public internet. The default listener is loopback.
The script installs Docker Engine/Compose if needed, generates a strong panel
password in `.env` (mode 600), builds and starts the service, and waits for health.
Reruns retain configuration and stored accounts. It never executes a remote
installation script. Preview the arguments with `./install.sh --check ...`.

For public access, point DNS at the server and use:

```sh
sudo ./install.sh --domain sessions.example.org
```

Caddy handles HTTPS automatically; ports 80 and 443 must be reachable. On an
existing installation edit `PUBLIC_URL`, `DOMAIN`, and `BIND_ADDRESS` in `.env`
before switching to the HTTPS profile. The API stays bound to loopback when
installed with `--domain`.

The Docker/Playwright setup follows the [Docker Engine installation docs](https://docs.docker.com/engine/install/ubuntu/)
and [Playwright container docs](https://playwright.dev/python/docs/docker).
One application process owns browser jobs and the update feed. Do not increase
Uvicorn workers or run multiple instances against the same SQLite volume.

## Panel and phones

1. Open `PUBLIC_URL`; sign in using `ADMIN_PASSWORD` from `.env`.
2. Add an account. Scan its MIREA QR or copy the ten-minute enrollment link to
   the owner. Owners never need the panel password or a device key.
3. Create a distinct device key for each phone. Grant all accounts or specific
   accounts. Grant publishing only to phones allowed to upload renewed sessions
   and updated passes; read-only phones can still use synchronized passes.
4. In the app open **Settings → Session server**, enter the URL/key and connect.
   Publisher phones can upload their existing local accounts and passes.
5. Re-login once in the panel when a university session needs owner approval.
   Connected apps receive a version event and fetch the new metadata/pass.

The server cannot inspect another Android app's private storage. This app
publishes its own login/session and NFC enrollment changes. A durable event feed
notifies connected apps immediately; disconnected or OS-suspended apps catch up
when they resume. No Firebase account or third-party push provider is required.
This is not guaranteed background delivery to an app that the OS has stopped.

NFC pass identifiers are shared, not short-lived JWTs. Each activation requests a
fresh Pulse token through the server. Gate admission remains the university's
choice. Ordinary QR/nearby transfer can also carry a saved pass with its account.

## Data, revocation and concurrency

The named `session_data` volume stores SQLite and `master.key`. Cookie headers,
browser/SSO storage and pass identifiers are encrypted with authenticated Fernet
encryption; token keys and the panel password are hashed. Browser working files
live in container tmpfs. Back up the database **and** `master.key` together;
losing the key means re-enrolling accounts. Never commit `.env` or data backups.

Updates use server-assigned revisions and compare-and-swap. An offline phone's
old revision cannot overwrite a newer pass. Opaque Pulse cookies do not expose
an issuance order, so an unlinked phone adopts an existing valid server session
instead of replacing it blindly. HTTP 403, service outages and gateway HTML do
not mean a session expired. Account identity is verified before replacement.

Each device key is separately revocable in the panel. API requests are scoped to
its allowed accounts. Login jobs have random, expiring capability links. Panel
writes require CSRF tokens; cookies are HttpOnly/SameSite Strict. There is no
arbitrary URL proxy, upstream credentials are never returned, and access logging
is disabled to keep enrollment-link tokens out of logs.

```sh
docker compose logs --tail=100 sessions
docker compose up -d --build --wait   # update after pulling reviewed changes
docker compose stop                  # stop without deleting data
```

Do not use `docker compose down -v` unless you intend to erase every stored
session/pass. Periodic read-only profile validation defaults to five minutes;
`REFRESH_SECONDS` can increase it. Keep time synchronized on the server.

## Tests

```sh
python3 -m venv .venv
.venv/bin/pip install -r requirements-test.txt
.venv/bin/pytest -q tests
bash -n install.sh
./install.sh --check --url http://192.168.1.20:8080 --bind 0.0.0.0
```

A live owner-approved login must still be tested with a real account after
installation. Tests use simulated Pulse responses and never enroll real users.
