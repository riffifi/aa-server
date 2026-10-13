'use strict';
const $ = id => document.getElementById(id);
let csrf = '', currentJob = null, accounts = [], syncing = false;
function notice(message) { $('notice').textContent = message; }
async function api(path, method = 'GET', body) {
  const headers = { 'Content-Type': 'application/json' };
  if (method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch('/panel/api/' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store' });
  const result = await response.json();
  if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Request failed');
  return result;
}
function el(tag, text, className) { const node = document.createElement(tag); if (text) node.textContent = text; if (className) node.className = className; return node; }
function action(text, run, className = 'quiet') {
  const button = el('button', text, className);
  button.addEventListener('click', async () => { button.disabled = true; try { await run(); } catch (error) { notice(error.message); } finally { button.disabled = false; } });
  return button;
}
function date(value) { return value ? new Date(value * 1000).toLocaleString() : 'unknown'; }
function renderAccount(account) {
  const row = el('div', null, 'row'), detail = el('div', null, 'row-body'), actions = el('div', null, 'actions');
  detail.append(el('strong', account.label), el('span', account.state, 'badge ' + account.state));
  if (account.pass_version) detail.append(el('span', 'Pass synced · v' + account.pass_version, 'badge'));
  detail.append(el('div', `Session expiry: ${date(account.expires_at)} · checked: ${date(account.checked_at)}`, 'meta'));
  actions.append(action('Check', async () => { await api('accounts/' + account.id + '/refresh', 'POST'); await refresh(); }));
  actions.append(action('Re-login', () => startLogin(account.label, account.id)));
  actions.append(action('Remove', async () => { if (confirm('Remove this account and its stored pass from this server?')) { await api('accounts/' + account.id, 'DELETE'); await refresh(); } }, 'quiet danger'));
  row.append(detail, actions); return row;
}
async function refresh() {
  if (syncing) return;
  syncing = true;
  try {
    const data = await api('state'); csrf = data.csrf; accounts = data.accounts;
    $('sign-in').hidden = true; $('panel').hidden = false;
    $('account-count').textContent = String(accounts.length); $('empty-accounts').hidden = accounts.length > 0;
    $('accounts').replaceChildren(...accounts.map(renderAccount));
    $('devices').replaceChildren(...data.devices.map(device => {
      const row = el('div', null, 'row'), details = el('div', null, 'row-body');
      details.append(el('strong', device.name), el('div', `${device.revoked ? 'Revoked' : 'Active'} · ${device.publisher ? 'Can publish' : 'Read and use'} · last seen ${date(device.last_seen)}`, 'meta'));
      row.append(details);
      if (!device.revoked) row.append(action('Revoke', async () => { if (confirm('Revoke this phone’s access?')) { await api('devices/' + device.id, 'DELETE'); await refresh(); } }, 'quiet danger'));
      return row;
    }));
  } catch (error) { if ($('panel').hidden === false) notice(error.message); }
  finally { syncing = false; }
}
async function startLogin(label, account_id) {
  const result = await api('logins', 'POST', { label, account_id }); currentJob = result.id;
  $('enroll-title').textContent = label; $('owner-link').value = location.origin + result.enrollment_path;
  $('enroll-card').hidden = false; $('qr').hidden = true; $('qr-link').hidden = true;
  $('enroll-status').textContent = 'Preparing a private browser session…';
  $('enroll-card').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  await pollLogin();
}
async function pollLogin() {
  const job = currentJob; if (!job) return;
  try {
    const result = await api('logins/' + job);
    if (job !== currentJob) return;
    $('enroll-status').textContent = result.error || ({ waiting: 'Waiting for a browser slot…', login: 'Waiting for the account owner’s approval…', complete: 'Account connected. Phones will fetch the update.', expired: 'QR login expired. Start a new login.', failed: 'Login failed. Try again.' }[result.status] || result.status);
    if (result.qr) { $('qr').src = '/panel/api/logins/' + job + '/qr'; $('qr').hidden = false; $('qr-link').href = result.qr; $('qr-link').hidden = false; }
    if (['complete', 'expired', 'failed', 'cancelled'].includes(result.status)) { currentJob = null; $('qr').hidden = true; $('qr-link').hidden = true; await refresh(); return; }
  } catch (error) { notice(error.message); }
  if (currentJob === job) setTimeout(pollLogin, 2000);
}
async function copy(input) {
  const element = $(input);
  try { if (navigator.clipboard) await navigator.clipboard.writeText(element.value); else { element.select(); if (!document.execCommand('copy')) throw Error(); } notice('Copied'); }
  catch (_) { element.focus(); element.select(); notice('Select and copy the text.'); }
}
$('login-form').addEventListener('submit', async event => { event.preventDefault(); try { csrf = (await api('login', 'POST', { password: $('password').value })).csrf; $('password').value = ''; notice(''); await refresh(); } catch (error) { notice(error.message); } });
$('new-account').onclick = () => $('account-dialog').showModal();
$('account-form').addEventListener('submit', async event => { event.preventDefault(); try { await startLogin($('account-label').value); $('account-dialog').close(); $('account-form').reset(); } catch (error) { notice(error.message); } });
$('new-device').onclick = () => {
  $('device-scopes').replaceChildren(...accounts.map(account => { const label = el('label', null, 'check'), input = document.createElement('input'); input.type = 'checkbox'; input.value = account.id; input.className = 'scope'; label.append(input, document.createTextNode(account.label)); return label; }));
  $('device-scopes').hidden = $('all-accounts').checked; $('device-dialog').showModal();
};
$('all-accounts').onchange = () => { $('device-scopes').hidden = $('all-accounts').checked; };
$('device-form').addEventListener('submit', async event => { event.preventDefault(); try {
  const scopes = $('all-accounts').checked ? ['*'] : [...document.querySelectorAll('.scope:checked')].map(input => input.value);
  const result = await api('devices', 'POST', { name: $('device-name').value, scopes, publisher: $('publisher').checked });
  $('device-dialog').close(); $('server-url').value = location.origin; $('device-key').value = result.key; $('key-dialog').showModal(); await refresh();
} catch (error) { notice(error.message); } });
$('copy-key').onclick = () => copy('device-key'); $('copy-owner').onclick = () => copy('owner-link');
$('close-enroll').onclick = async () => { if (currentJob) await api('logins/' + currentJob, 'DELETE').catch(error => notice(error.message)); currentJob = null; $('enroll-card').hidden = true; };
$('sign-out').onclick = async () => { try { await api('logout', 'POST'); $('panel').hidden = true; $('sign-in').hidden = false; $('enroll-card').hidden = true; currentJob = null; csrf = ''; } catch (error) { notice(error.message); } };
document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => { if (button.closest('dialog').id === 'key-dialog') $('device-key').value = ''; button.closest('dialog').close(); }; });
refresh(); setInterval(() => { if (!$('panel').hidden) refresh(); }, 5000);
