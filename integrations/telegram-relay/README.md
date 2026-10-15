# Telegram API relay on Cloudflare Workers

Use this when the origin server cannot establish an outbound connection to
Telegram. The domain's ordinary Cloudflare proxy handles incoming requests;
a Worker can issue the outgoing Bot API requests from Cloudflare's network.

The Worker accepts only authenticated POSTs for the bot methods used by this
server. It has one fixed upstream and one configured bot, limits bodies to 1 MiB,
preserves multipart QR uploads, and does not forward its relay authorization
header to Telegram. The bot token stays in a Worker secret, outside incoming
URLs. No DNS, mail, other application, or server-wide VPN changes are required.

## Deploy

The included `worker.mjs` and `wrangler.toml` are ready to deploy to a `workers.dev`
address in the domain owner's Cloudflare account. Cloudflare account access is
required; no Cloudflare connector is connected in this workspace.

For an agent-assisted deployment, provide the account ID and a private local file
containing an API token scoped to **Workers Scripts: Edit** for that account.
Do not paste credentials into source or commit them. Alternatively, create a
Worker in the Cloudflare dashboard, paste `worker.mjs`, and configure these two
**secret** bindings before activation:

- `BOT_TOKEN`: the bot's current token, matching the server's `TELEGRAM_TOKEN`.
- `RELAY_KEY`: a separate randomly generated secret, shared only with the server.

A relay key can be generated in a private terminal using
`python3 -c 'import secrets; print(secrets.token_urlsafe(32))'`.
Disable Worker logs/traces, as in the supplied Wrangler configuration; never log
secret bindings, authorization headers, or Telegram upstream URLs.

Keep the existing server configuration and add to its root-owned `.env`:

```dotenv
TELEGRAM_RELAY_URL=https://antiattendance-telegram-relay.YOUR-SUBDOMAIN.workers.dev
TELEGRAM_RELAY_KEY=<the-generated-relay-key>
```

The Worker URL must use HTTPS, without credentials or query parameters. Retain
`TELEGRAM_TOKEN`; it enables the bot and remains available for a direct route.
After updating the server source, run `sudo docker compose up -d --build --wait`
from the server checkout. The optional relay settings affect only Telegram;
Pulse/MIREA and the application's incoming API keep their existing route.

When ready, the bot discovers its username through the relay and accepts
`/menu`. Verify a real owner login, a second person's request, Allow/Deny, and
revocation. A healthy application container alone does not prove Telegram is
reachable. Never start a second `getUpdates` poller to test the same bot.

## Tests and upstream references

```sh
node --test integrations/telegram-relay/worker.test.mjs
python -m pytest -q tests
```

Tests cover unauthorized callers, a fixed method allowlist, multipart forwarding,
body limits, safe upstream errors, and server-side relay configuration.

- [Cloudflare Worker request forwarding](https://developers.cloudflare.com/workers/examples/modify-request-property/)
- [Cloudflare Worker secret bindings](https://developers.cloudflare.com/workers/configuration/secrets/)
- [Telegram Bot API](https://core.telegram.org/bots/api)
