# Performance update

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
