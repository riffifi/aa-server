# Browser resource retention in the current Python services

Go browser work additionally has a 64 MiB / 128-entry shared public-asset cache,
a 32 MiB item limit, conditional ETag/Last-Modified revalidation and per-URL
download coalescing. It includes explicitly public `/_framework/` runtime files.
Private, cookie-setting and personalized responses never enter the shared cache.

A concrete repeated-download mechanism was found: the old public-asset cache
clamped TTL to 900 seconds, matching the default 900-second renewal interval.
Assets could expire before every renewal despite a longer upstream max-age.
The Python clamp is now 86400 seconds (still respecting a shorter upstream TTL),
and Go additionally revalidates expired files without downloading unchanged bodies.
Both caches allow public framework binaries; the Python cache also uses a 64 MiB
budget and a 32 MiB item limit, so larger runtime files can be reused.
This mechanism is supported by code and synthetic tests; it does not identify all
of the deployment's historical 200 GB without measurements from that deployment.

Go restarts an idle browser before the next context after 50 completed contexts or 30 minutes, without
interrupting an active context. This reclaims Chromium's retained native memory;
it is not evidence that all RSS growth was a leak. Response disposal, worker
cancellation and bounded cache/measurement queues handle application retention.

Authenticated panel endpoint `GET /panel/api/debug/browser` exposes heap and
goroutine counts, active contexts, cache bytes/hits/revalidations, saved decoded
bytes, declared Content-Length totals, unknown-length counts and aggregate page
request sizes by host/resource type. No URLs, QR tokens, cookies or bodies are
retained by telemetry. Counters reset on process restart. Response measurements
are not packet-level interface accounting; dropped/unknown measurements are
reported explicitly. AA ID has its own engine counters internally.

The audit found avoidable retention, not a measured explanation of all resident
Chromium memory on the deployment. Chromium may keep allocated memory even
after a context is closed; RSS alone does not prove a leak.

Fixed in both browser-login implementations:

- Explicitly dispose each APIResponse after reading the body, also on errors,
  oversized payloads, non-200 QR poll replies and request cancellation. Playwright
  otherwise retains those bodies until the request context closes.
- Close a newly allocated browser context if route/timeout setup fails.
- Periodically expire old login jobs even if no subsequent login starts. This
  also releases unclaimed AA ID browser state after the existing retention time.
- Track cancelled expired jobs until completion and drain them during shutdown.

In the sessions diagnostic recorder, finished response tasks no longer accumulate
in a list. Cancellation removes the response listener, cancels/drains pending
readers and closes the context before returning.

Checks use simulated Playwright responses and contexts, including cancellation
and failures. Live QR/login/renewal and long-term RSS still require deployment
verification. No live-server access was used for this audit.

The API and existing job retention periods are preserved. A cleanup timer checks
expiry once per minute. Deploy the updated source with the normal application
container rebuild, preserving `.env`, data volumes and key files. This fix does
not require PostgreSQL, the Go migration or changes to Nginx.
