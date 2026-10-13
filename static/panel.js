'use strict';
const $ = id => document.getElementById(id);
let csrf = '', currentJob = null, accounts = [], syncing = false, editingGroup = null;
function notice(message) { $('notice').textContent = message; }
async function api(path, method = 'GET', body) {
  const headers = { 'Content-Type': 'application/json' };
  if (method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch('/panel/api/' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store' });
  if (response.status === 401 && path !== 'login') { csrf = ''; currentJob = null; $('panel').hidden = true; $('sign-in').hidden = false; $('enroll-card').hidden = true; }
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
  detail.append(el('div', account.automatic_renewal ? `Automatic renewal enabled · last renewal: ${date(account.renewed_at)}` : 'Re-login here once to enable automatic renewal.', 'meta'));
  detail.append(el('div', account.group_id ? `Group: ${account.group_name || account.group_id}` : 'No group assigned', 'meta'));
  actions.append(action('Group', async () => { editingGroup = account; $('group-account').textContent = account.label; $('group-id').value = account.group_id || ''; $('group-name').value = account.group_name || ''; $('group-dialog').showModal(); }));
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
    $('server-url').value = location.origin;
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
    $('enroll-status').textContent = result.error || ({ waiting: 'Waiting for a browser slot…', login: 'Waiting for the account owner’s approval…', finishing: 'Approval received. Confirming the Pulse session…', complete: 'Account connected. Phones will fetch the update.', expired: 'QR login expired. Start a new login.', failed: 'Login failed. Try again.' }[result.status] || result.status);
    if (result.qr) { $('qr').src = '/panel/api/logins/' + job + '/qr'; $('qr').hidden = false; $('qr-link').href = result.qr; $('qr-link').hidden = false; }
    if (!result.qr) { $('qr').hidden = true; $('qr-link').hidden = true; }
    if (['complete', 'expired', 'failed', 'cancelled'].includes(result.status)) { currentJob = null; $('qr').hidden = true; $('qr-link').hidden = true; await refresh(); return; }
  } catch (error) { notice(error.message); }
  if (currentJob === job) setTimeout(pollLogin, 2000);
}
async function copy(input) {
  const element = $(input);
  try { if (navigator.clipboard) await navigator.clipboard.writeText(element.value); else { element.select(); if (!document.execCommand('copy')) throw Error(); } notice('Copied'); }
  catch (_) { element.focus(); element.select(); notice('Select and copy the text.'); }
}
$('group-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (!editingGroup) return;
  const raw = $('group-id').value.trim(), id = raw ? Number(raw) : null;
  if (id !== null && (!Number.isSafeInteger(id) || id <= 0)) { notice('Enter a valid schedule group ID.'); return; }
  try {
    await api('accounts/' + editingGroup.id + '/group', 'PUT', { group_id: id, group_name: id === null ? null : $('group-name').value.trim(), expected_version: editingGroup.version });
    $('group-dialog').close(); editingGroup = null; await refresh(); notice('Group saved. Phones will sync the change.');
  } catch (error) { notice(error.message); }
});
$('login-form').addEventListener('submit', async event => { event.preventDefault(); try { csrf = (await api('login', 'POST', { password: $('password').value })).csrf; $('password').value = ''; notice(''); await refresh(); } catch (error) { notice(error.message); } });
$('new-account').onclick = () => $('account-dialog').showModal();
$('account-form').addEventListener('submit', async event => { event.preventDefault(); try { await startLogin($('account-label').value); $('account-dialog').close(); $('account-form').reset(); } catch (error) { notice(error.message); } });
$('copy-server').onclick = () => copy('server-url'); $('copy-owner').onclick = () => copy('owner-link');
$('close-enroll').onclick = async () => { if (currentJob) await api('logins/' + currentJob, 'DELETE').catch(error => notice(error.message)); currentJob = null; $('enroll-card').hidden = true; };
$('sign-out').onclick = async () => { try { await api('logout', 'POST'); $('panel').hidden = true; $('sign-in').hidden = false; $('enroll-card').hidden = true; currentJob = null; csrf = ''; } catch (error) { notice(error.message); } };
document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => { button.closest('dialog').close(); }; });
refresh(); setInterval(() => { if (!$('panel').hidden) refresh(); }, 5000);
