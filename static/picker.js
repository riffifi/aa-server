'use strict';
const $ = id => document.getElementById(id);
let currentJob = null, currentBase = null;

// Telegram injects window.Telegram.WebApp only when the page is opened as a mini
// app (and its script is reachable). Everything below works as a normal web page
// whether or not that is present; this is a progressive enhancement only.
try {
  const webapp = window.Telegram && window.Telegram.WebApp;
  if (webapp) { webapp.ready(); webapp.expand(); }
} catch (_) {}

function notice(message) { $('notice').textContent = message || ''; }
function el(tag, text, className) { const n = document.createElement(tag); if (text) n.textContent = text; if (className) n.className = className; return n; }
function date(value) { return value ? new Date(value * 1000).toLocaleString() : 'unknown'; }

const STATE_LABEL = { valid: 'Connected', expired: 'Needs re-login', forbidden: 'Blocked by MIREA', unavailable: 'Check later', invalidResponse: 'Check later' };

async function loadAccounts() {
  $('load-error').hidden = true;
  try {
    const response = await fetch('/api/accounts', { cache: 'no-store' });
    if (!response.ok) throw Error('Could not load accounts. Try again.');
    const data = await response.json();
    const accounts = data.accounts.slice().sort((a, b) => (a.state === 'expired') === (b.state === 'expired') ? a.label.localeCompare(b.label) : a.state === 'expired' ? -1 : 1);
    $('account-count').textContent = String(accounts.length);
    $('empty-accounts').hidden = accounts.length > 0;
    $('accounts').replaceChildren(...accounts.map(renderAccount));
  } catch (error) {
    $('load-error').textContent = error.message;
    $('load-error').hidden = false;
  }
}

function renderAccount(account) {
  const row = el('div', null, 'row'), body = el('div', null, 'row-body'), actions = el('div', null, 'actions');
  const name = el('a', account.label, 'inline');
  name.href = '/u/' + encodeURIComponent(account.id);
  const strong = el('strong');
  strong.append(name);
  body.append(strong, el('span', STATE_LABEL[account.state] || account.state, 'badge ' + account.state));
  if (account.group_id) body.append(el('span', account.group_name || ('Group ' + account.group_id), 'badge'));
  body.append(el('div', account.state === 'valid' ? `Renews automatically · session until ${date(account.expires_at)}` : 'Tap Renew and confirm in the MIREA app.', 'meta'));
  const page = el('button', 'Моя страница', 'quiet');
  page.addEventListener('click', () => { location.href = '/u/' + encodeURIComponent(account.id); });
  const renew = el('button', account.state === 'valid' ? 'Renew now' : 'Renew', account.state === 'valid' ? 'secondary' : '');
  renew.addEventListener('click', () => startLogin(account.label, account.id));
  actions.append(page, renew);
  row.append(body, actions);
  return row;
}

async function startLogin(label, accountId) {
  notice('');
  $('login-card').hidden = false;
  $('login-title').textContent = label;
  $('login-status').textContent = 'Preparing a secure login…';
  $('qr').hidden = true; $('qr-link').hidden = true;
  $('login-card').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  try {
    const response = await fetch('/api/logins', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ label, account_id: accountId || null }), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Could not start login.');
    currentBase = result.enrollment_path;
    currentJob = result.id;
    pollLogin();
  } catch (error) {
    $('login-status').textContent = error.message;
  }
}

async function pollLogin() {
  const job = currentJob, base = currentBase;
  if (!job) return;
  try {
    const response = await fetch(base + '/state', { cache: 'no-store' });
    if (!response.ok) throw Error('This login expired. Start again.');
    const result = await response.json();
    if (job !== currentJob) return;
    $('login-status').textContent = result.error || ({
      waiting: 'Waiting for a browser slot…',
      login: 'Scan the QR with the MIREA app, or open the link below in a signed-in MIREA browser.',
      finishing: 'Approval received. Confirming your Pulse session…',
      complete: 'Done — your account is connected and will renew automatically.',
      expired: 'The login expired. Start again.',
      failed: 'Login failed. Start again.',
      cancelled: 'Login cancelled.'
    }[result.status] || result.status);
    const image = $('qr'), link = $('qr-link');
    if (result.qr) { image.src = base + '/qr?t=' + Date.now(); image.hidden = false; link.href = result.qr; link.hidden = false; }
    else { image.hidden = true; link.hidden = true; }
    if (['complete', 'expired', 'failed', 'cancelled'].includes(result.status)) {
      currentJob = null;
      if (result.status === 'complete') { notice('Account connected.'); await loadAccounts(); }
      return;
    }
  } catch (error) {
    $('login-status').textContent = error.message;
    currentJob = null;
    return;
  }
  if (currentJob === job) setTimeout(pollLogin, 2000);
}

$('add-account').onclick = () => $('name-dialog').showModal();
$('reload').onclick = () => loadAccounts();
$('close-login').onclick = () => { currentJob = null; $('login-card').hidden = true; };
$('name-form').addEventListener('submit', event => {
  event.preventDefault();
  const label = $('new-label').value.trim();
  if (!label) return;
  $('name-dialog').close();
  $('name-form').reset();
  startLogin(label, null);
});
document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => button.closest('dialog').close(); });

loadAccounts();
setInterval(() => { if (!currentJob) loadAccounts(); }, 8000);
