import assert from 'node:assert/strict';
import { test, afterEach } from 'node:test';
import worker from './worker.mjs';

const env = { BOT_TOKEN: 'test-bot-token', RELAY_KEY: 'a-private-test-relay-key-at-least-32-characters' };
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
const request = (path = '/getMe', options = {}) => new Request('https://relay.example' + path, {
  method: 'POST', headers: { authorization: `Bearer ${env.RELAY_KEY}` }, ...options,
});

test('missing configuration and unauthorized callers cannot reach Telegram', async () => {
  globalThis.fetch = () => { throw new Error('Must not forward'); };
  assert.equal((await worker.fetch(request(), {})).status, 503);
  assert.equal((await worker.fetch(request('/getMe', { headers: {} }), env)).status, 401);
  assert.equal((await worker.fetch(request('/getMe', { headers: { authorization: 'wrong' } }), env)).status, 401);
});

test('only fixed POST bot methods are forwarded', async () => {
  globalThis.fetch = () => { throw new Error('Must not forward'); };
  for (const path of ['/setWebhook', '/sendMessage?url=https://evil.example', '/https://evil.example', '/bot-other/getMe']) {
    assert.equal((await worker.fetch(request(path), env)).status, 404);
  }
  assert.equal((await worker.fetch(request('/getMe', { method: 'GET' }), env)).status, 405);
});

test('multipart data is preserved, relay authorization stays at the relay', async () => {
  globalThis.fetch = async (url, init) => {
    assert.equal(url, 'https://api.telegram.org/bottest-bot-token/sendPhoto');
    assert.equal(init.headers.get('authorization'), null);
    assert.equal(init.headers.get('content-type'), 'multipart/form-data; boundary=sample');
    assert.equal(new TextDecoder().decode(init.body), '--sample\r\nphoto\r\n--sample--');
    assert.equal(init.redirect, 'error');
    return Response.json({ ok: true, result: { message_id: 1 } });
  };
  const result = await worker.fetch(request('/sendPhoto', {
    headers: { authorization: `Bearer ${env.RELAY_KEY}`, 'content-type': 'multipart/form-data; boundary=sample' },
    body: '--sample\r\nphoto\r\n--sample--',
  }), env);
  assert.equal(result.status, 200);
  assert.equal(result.headers.get('cache-control'), 'no-store');
  assert.equal((await result.json()).ok, true);
});

test('oversized bodies and upstream errors do not leak credentials', async () => {
  globalThis.fetch = async () => { throw new Error(env.BOT_TOKEN); };
  assert.equal((await worker.fetch(request('/sendPhoto', { body: 'x'.repeat(1024 * 1024 + 1) }), env)).status, 413);
  const result = await worker.fetch(request(), env);
  assert.equal(result.status, 502);
  assert.equal((await result.text()).includes(env.BOT_TOKEN), false);
});
