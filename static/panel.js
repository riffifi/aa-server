'use strict';
const $ = id => document.getElementById(id);
let testingPasses = false;
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
  detail.append(el('span', account.page_seen_at ? 'Входил ✓' : 'Не входил', 'badge ' + (account.page_seen_at ? 'valid' : '')));
  if (account.page_seen_at) detail.append(el('div', `Последний вход на страницу: ${date(account.page_seen_at)}`, 'meta'));
  actions.append(action('Group', async () => { editingGroup = account; $('group-account').textContent = account.label; $('group-id').value = account.group_id || ''; $('group-name').value = account.group_name || ''; $('group-dialog').showModal(); }));
  actions.append(action('Rename', async () => { const name = prompt('Новое название аккаунта', account.label); if (name && name.trim()) { await api('accounts/' + account.id + '/label', 'PUT', { label: name.trim(), expected_version: account.version }); await refresh(); } }));
  actions.append(action('Check', async () => { await api('accounts/' + account.id + '/refresh', 'POST'); await refresh(); }));
  actions.append(action('Посещение', async () => { const d = await api('accounts/' + account.id + '/attendance'); const t = s => new Date(s * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); const lines = (d.events || []).map(e => (e.direction === 'in' ? '→' : '←') + t(e.time)).join(', '); notice(`${account.label}: ${d.inside ? 'в здании' : 'снаружи'}${lines ? ' · ' + lines : ' · сегодня проходов нет'}`); }));
  actions.append(action('Re-login', () => startLogin(account.label, account.id)));
  actions.append(action('Завершить сессию', async () => { if (confirm('Завершить сессию Пульса для «' + account.label + '»? Останется в списке, но потребует повторного входа.')) { await api('accounts/' + account.id + '/end-session', 'POST'); await refresh(); } }, 'quiet danger'));
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
    $('pass-test-password').value = data.pass_testing.password;
    $('pass-test-expiry').textContent = 'Rotates at ' + date(data.pass_testing.expires_at) + ' (midnight in Moscow).';
    if (testingPasses) await previewTestPasses();
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
$('sign-out').onclick = async () => { testingPasses = false; $('pass-test-password').value = ''; $('pass-test-status').textContent = ''; try { await api('logout', 'POST'); $('panel').hidden = true; $('sign-in').hidden = false; $('enroll-card').hidden = true; currentJob = null; csrf = ''; } catch (error) { notice(error.message); } };
document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => { button.closest('dialog').close(); }; });
refresh(); setInterval(() => { if (!$('panel').hidden) refresh(); }, 5000);

async function previewTestPasses() {
  const response = await fetch('/api/accounts', {
    headers: { 'X-Pass-Test-Key': $('pass-test-password').value }, cache: 'no-store',
  });
  if (!response.ok) throw Error('Could not preview pass testing.');
  const result = await response.json();
  const count = (result.passes || []).filter(pass => pass.number).length;
  $('pass-test-status').textContent = `${count} stored passes available for testing. Owner agreement settings are unchanged.`;
}
$('copy-pass-test').onclick = () => copy('pass-test-password');
$('toggle-pass-test').onclick = async () => {
  testingPasses = !testingPasses;
  $('toggle-pass-test').setAttribute('aria-pressed', String(testingPasses));
  $('toggle-pass-test').textContent = testingPasses ? 'Stop test preview' : 'Preview test passes';
  if (!testingPasses) { $('pass-test-status').textContent = ''; return; }
  try { await previewTestPasses(); } catch (error) { notice(error.message); }
};
