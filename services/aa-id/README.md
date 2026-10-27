# AA ID

AA ID is the account service for AntiAttendance. It is deliberately **not** part of
the session server: it has its own process, SQLite database, signing key and
Docker image, and imports nothing from `app/`. The session server only verifies
AA ID tokens offline with the public key from `/.well-known/jwks.json`.

## Rules

- **One AA ID per person.** An AA ID is created on the first MIREA sign-in and is
  bound to that person's Pulse identity. `mirea_links.pulse_uid` is `UNIQUE` and
  `mirea_links.user_id` is the primary key, so one MIREA account can never back two
  AA IDs and one AA ID can never hold two MIREA accounts.
- **No shared accounts.** Every new device must prove MIREA ownership again. An AA
  ID has at most `MAX_SESSIONS` (default 2) signed-in devices; a new sign-in signs
  out the least recently used one, so handing your MIREA login to someone else
  signs you out. Refresh tokens rotate on every use, and replaying an old refresh
  token (a copied token) revokes that device session. Owners list and revoke their
  devices; an admin can block an AA ID.
- **No MIREA credentials are kept.** The Pulse cookie from the device is used once
  to ask Pulse `GetMeInfo` (who is this) and `GetSubscribedSchedules` (which group)
  and is then dropped. It is never written to the database or logs.

## API

All bodies are JSON. Behind the bundled Nginx the service lives at `PUBLIC_URL/id`.

| Method | Path | |
|---|---|---|
| GET | `/.well-known/jwks.json` | Ed25519 public key, issuer and audience |
| POST | `/v1/auth/mirea` | `{pulse_cookie, device}` → `{access_token, access_expires_at, refresh_token, session_id, user}` |
| POST | `/v1/auth/refresh` | `{refresh_token}` → same shape, new refresh token |
| POST | `/v1/auth/logout` | Bearer; ends this device session |
| GET | `/v1/me` | Bearer; the person's AA ID, group, device limit |
| GET | `/v1/me/sessions` | Bearer; signed-in devices |
| DELETE | `/v1/me/sessions/{id}` | Bearer; sign a device out |
| GET | `/` | the AA ID website (see below) |
| PUT | `/v1/admin/users/{id}/status` | `X-Admin-Token`; `{status: active|blocked}` (disabled without `ADMIN_TOKEN`) |

Access tokens are EdDSA JWTs valid for `ACCESS_TTL_SECONDS` (default 600):
`iss=aa-id`, `aud=aa`, `sub` (AA ID), `sid` (device session), `mirea` (Pulse person
id), `grp` (group id or null), `iat`, `exp`. Services that verify offline see a
revocation within one access-token lifetime; AA ID's own endpoints see it at once.

## Website

The service also serves the AA ID site at `/`: sign in with the MIREA QR code
(a headless browser on the server runs the MIREA login, reads the Pulse person id
and group, signs that MIREA session out and closes), then see the account and the
signed-in devices and sign any of them out. It also shows the person's turnstile
entries and exits for the last week (newest first; read from the session server's
`GET /api/me/attendance` with a one-minute AA ID token, only when that MIREA account
is connected there) and today's current or next lesson of their group from
schedule-of.mirea.ru, with a link to the group's schedule. The site uses an `HttpOnly`,
`SameSite=Strict` cookie; one browser is signed in at a time and site sessions do
not count towards the phone limit. Site endpoints: `POST /web/logins`,
`GET /web/logins/{id}` and `POST /web/logins/{id}/claim` (both with the
`X-Login-Token` the start call returned), `GET /web/me`, `DELETE /web/sessions/{id}`,
`GET /web/attendance`, `GET /web/schedule`, `POST /web/logout`.

The page follows the Brockmann design language (riffifi/brockmann-flutter): Geist
typography, black or white surfaces, one accent, square edges and hairline divisions.
The Geist fonts are bundled under their OFL licence (`aaid/static/fonts/OFL.txt`).

### Own domain

`compose.yaml` publishes AA ID on `127.0.0.1:${AAID_PORT:-8091}` (besides `/id/` on
the main server). For a third-level domain such as `id.yellowteacrew.ru`:

1. Point the domain's DNS at the server (or add it in Cloudflare).
2. Copy `nginx/aaid-host.conf.example` into the host Nginx, adjust `server_name`,
   add a certificate (`certbot --nginx -d id.yellowteacrew.ru`) and reload Nginx.
3. Put `AAID_PUBLIC_URL=https://id.yellowteacrew.ru` in `.env` (secure cookies, and
   the session server advertises this address to the app), then
   `docker compose up -d --build`.

## Session server integration

The session server verifies AA ID tokens with the keys from `AAID_JWKS_URL`
(Compose points it at the `aaid` container). With `AAID_ENFORCE=1` it rejects every
`/api/*` request without a valid AA ID bearer token (`401`, `X-AAID-Required: 1`). The
AntiAttendance site (2.0) signs people in with AA ID: `?continue=aa&state=…` on this
site shows "Продолжить в AntiAttendance", `POST /web/authorize` issues a one-time code
(60 s) sent only to `AA_SITE_URL/auth/callback`, and the session server trades it at
`POST /v1/site/exchange` for the person (id, MIREA id, group). A QR sign-in on this site
also hands the fresh MIREA session to the session server (`POST /internal/aaid/link`,
signed with audience `aa-link`), so the person's account appears there without a second
QR; AA ID itself still stores no MIREA cookie. With `fresh=1` the site asks for a new QR
sign-in even when signed in. `GET /health` reports `aaid.required` and
`aaid.url` so the app can find AA ID. `GET /api/accounts` marks the caller's own
account with `own: true` (same Pulse identity).

### Rollout

1. Deploy: `docker compose up -d --build` starts `aaid` next to `sessions`, and
   Nginx routes `/id/` to it. Nothing is enforced yet.
2. Release the app version with the AA ID gate and let phones update.
3. Enforce: put `AAID_ENFORCE=1` in `.env` and run `docker compose up -d`. Older
   app builds then stop working against the server.

Back up the `aaid_data` volume (`aaid.sqlite3` and `signing.key`) together. Losing
`signing.key` only signs everyone out; losing the database loses the AA ID ↔ MIREA
bindings (people get new AA IDs on their next sign-in).

## Data model and lesson sharing (planned)

Current tables: `users`, `mirea_links` (with the person's `group_id`), `sessions`,
`refresh_tokens`, `audit`.

Sharing access with people who attend the **same lesson** (usually groupmates,
sometimes another group in a joint lecture) is designed to be added without
changing the existing tables:

- `circles(id, kind, group_id, title, created_by, created)` — a set of people; kind
  `group` is derived from `mirea_links.group_id`, kind `custom` is hand-made for
  cross-group lessons.
- `circle_members(circle_id, user_id, role, added_at)`.
- `grants(id, grantor_user_id, grantee_user_id, lesson_key, starts_at, ends_at,
  created, revoked_at)` — permission scoped to one lesson occurrence. `lesson_key`
  is the schedule lesson id plus its date, so a grant only works while that lesson
  runs and only between people who both have it.

Because both sides are AA IDs (and so verified MIREA people), a grant always names
real individuals, and the session server can check it from token claims (`sub`,
`grp`) plus a grant lookup.

## Development

```
pip install -r requirements-test.txt
python -m pytest
```
