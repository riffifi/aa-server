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
| PUT | `/v1/admin/users/{id}/status` | `X-Admin-Token`; `{status: active|blocked}` (disabled without `ADMIN_TOKEN`) |

Access tokens are EdDSA JWTs valid for `ACCESS_TTL_SECONDS` (default 600):
`iss=aa-id`, `aud=aa`, `sub` (AA ID), `sid` (device session), `mirea` (Pulse person
id), `grp` (group id or null), `iat`, `exp`. Services that verify offline see a
revocation within one access-token lifetime; AA ID's own endpoints see it at once.

## Session server integration

When `AAID_JWKS_URL` is set, the session server rejects every `/api/*` request
without a valid AA ID bearer token (`401`, `X-AAID-Required: 1`). The browser pages
keep working through their own MIREA sign-in: `POST /api/logins`,
`/api/groups/search`, the owner page endpoints, and owner endpoints of the account
whose `page_session` cookie is presented. `GET /health` reports `aaid.required` and
`aaid.url` so the app can find AA ID. `GET /api/accounts` marks the caller's own
account with `own: true` (same Pulse identity).

### Rollout

1. Deploy: `docker compose up -d --build` starts `aaid` next to `sessions`, and
   Nginx routes `/id/` to it. Nothing is enforced yet.
2. Release the app version with the AA ID gate and let phones update.
3. Enforce: put `AAID_JWKS_URL=http://aaid:8090/.well-known/jwks.json` in `.env`
   and rerun `./install.sh`. Older app builds then stop working against the server.

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
