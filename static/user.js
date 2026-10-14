'use strict';
const $ = id => document.getElementById(id);
const accountId = decodeURIComponent(location.pathname.replace(/^\/u\//, '').replace(/\/$/, ''));
let account = null, currentJob = null, currentBase = null, savingPass = false;

const STATE_LABEL = { valid: 'Connected', expired: 'Needs re-login', forbidden: 'Blocked by MIREA', unavailable: 'Check later', invalidResponse: 'Check later' };
function notice(message) { $('notice').textContent = message || ''; }
function date(value) { return value ? new Date(value * 1000).toLocaleString() : 'unknown'; }

async function load() {
  try {
    const response = await fetch('/api/accounts/' + encodeURIComponent(accountId), { cache: 'no-store' });
    if (response.status === 404) throw Error('This account was not found. Ask for your personal link again.');
    if (!response.ok) throw Error('Could not load your account. Try again.');
    account = await response.json();
    render();
  } catch (error) {
    $('error').textContent = error.message;
    $('error').hidden = false;
    $('card').hidden = true;
  }
}

function render() {
  $('card').hidden = false;
  $('error').hidden = true;
  $('title').textContent = account.label;
  $('label').textContent = account.label;
  const badge = $('state-badge');
  badge.textContent = STATE_LABEL[account.state] || account.state;
  badge.className = 'badge ' + account.state;
  $('meta').textContent = account.state === 'valid'
    ? `Renews automatically · session until ${date(account.expires_at)}`
    : 'Tap Renew and confirm in the MIREA app.';
  $('renew').textContent = account.state === 'valid' ? 'Renew now' : 'Renew';
  if (!savingPass) $('pass-allowed').checked = account.pass_allowed !== false;
}

async function savePass() {
  savingPass = true;
  const allowed = $('pass-allowed').checked;
  try {
    const response = await fetch('/api/accounts/' + encodeURIComponent(accountId) + '/pass-allowed', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ allowed, expected_version: account ? account.version : null }), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Could not save.');
    account = result;
    notice(allowed ? 'Pass enabled for the app.' : 'Pass disabled for the app.');
  } catch (error) {
    notice(error.message);
    $('pass-allowed').checked = account && account.pass_allowed !== false;
  } finally {
    savingPass = false;
  }
}

async function startLogin() {
  notice('');
  $('login-card').hidden = false;
  $('login-status').textContent = 'Preparing a secure login…';
  $('qr').hidden = true; $('qr-link').hidden = true;
  $('login-card').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  try {
    const response = await fetch('/api/logins', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ label: account.label, account_id: accountId }), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Could not start login.');
    currentBase = result.enrollment_path; currentJob = result.id;
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
      expired: 'The login expired. Start again.', failed: 'Login failed. Start again.', cancelled: 'Login cancelled.'
    }[result.status] || result.status);
    if (result.qr) { $('qr').src = base + '/qr?t=' + Date.now(); $('qr').hidden = false; $('qr-link').href = result.qr; $('qr-link').hidden = false; }
    else { $('qr').hidden = true; $('qr-link').hidden = true; }
    if (['complete', 'expired', 'failed', 'cancelled'].includes(result.status)) {
      currentJob = null;
      if (result.status === 'complete') { await load(); }
      return;
    }
  } catch (error) {
    $('login-status').textContent = error.message; currentJob = null; return;
  }
  if (currentJob === job) setTimeout(pollLogin, 2000);
}

$('renew').onclick = () => startLogin();
$('pass-allowed').onchange = () => savePass();
$('close-login').onclick = () => { currentJob = null; $('login-card').hidden = true; };

load();
setInterval(() => { if (!currentJob && !savingPass) load(); }, 8000);
