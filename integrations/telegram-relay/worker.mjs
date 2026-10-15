const methods = new Set([
  'getMe', 'getUpdates', 'deleteWebhook', 'setMyCommands',
  'sendMessage', 'editMessageText', 'sendPhoto', 'answerCallbackQuery',
]);
const limit = 1024 * 1024;

function reply(status, description) {
  return Response.json({ ok: false, description }, {
    status, headers: { 'cache-control': 'no-store' },
  });
}

async function matches(value, expected) {
  if (!value || value.length > 256) return false;
  const encoder = new TextEncoder();
  const [left, right] = await Promise.all([
    crypto.subtle.digest('SHA-256', encoder.encode(value)),
    crypto.subtle.digest('SHA-256', encoder.encode(expected)),
  ]);
  const a = new Uint8Array(left);
  const b = new Uint8Array(right);
  let difference = 0;
  for (let i = 0; i < a.length; i++) difference |= a[i] ^ b[i];
  return difference === 0;
}

export default {
  async fetch(request, env) {
    if (!env.BOT_TOKEN || !env.RELAY_KEY || env.RELAY_KEY.length < 32) {
      return reply(503, 'Relay is not configured');
    }
    if (!await matches(request.headers.get('authorization'), `Bearer ${env.RELAY_KEY}`)) {
      return reply(401, 'Unauthorized');
    }
    if (request.method !== 'POST') return reply(405, 'POST required');
    const url = new URL(request.url);
    const method = url.pathname.slice(1);
    if (url.search || !methods.has(method)) return reply(404, 'Method not allowed');
    const chunks = [];
    let length = 0;
    if (request.body) {
      const reader = request.body.getReader();
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        length += value.byteLength;
        if (length > limit) {
          await reader.cancel();
          return reply(413, 'Request too large');
        }
        chunks.push(value);
      }
    }
    const body = new Uint8Array(length);
    let offset = 0;
    for (const chunk of chunks) {
      body.set(chunk, offset);
      offset += chunk.byteLength;
    }
    const headers = new Headers();
    const contentType = request.headers.get('content-type');
    if (contentType) headers.set('content-type', contentType);
    try {
      const upstream = await fetch(`https://api.telegram.org/bot${env.BOT_TOKEN}/${method}`, {
        method: 'POST', headers, body, redirect: 'error', signal: AbortSignal.timeout(45000),
      });
      return new Response(upstream.body, {
        status: upstream.status,
        headers: {
          'content-type': upstream.headers.get('content-type') || 'application/json',
          'cache-control': 'no-store',
        },
      });
    } catch {
      return reply(502, 'Telegram is unavailable');
    }
  },
};
