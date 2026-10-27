'use strict';
const $ = id => document.getElementById(id);
let testingPasses = false;
let csrf = '', currentJob = null, accounts = [], syncing = false, editingGroup = null;
const STATE = { valid: ['Сессия работает', 'ok'], expired: ['Сессия истекла', 'bad'], forbidden: ['Доступ запрещён', 'bad'], unavailable: ['Пульс недоступен', 'wait'], invalidResponse: ['Непонятный ответ Пульса', 'wait'] };

function notice(message) { $('notice').textContent = message || ''; $('notice').hidden = !message; }
async function api(path, method = 'GET', body) {
  const headers = { 'Content-Type': 'application/json' };
  if (method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch('/panel/api/' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store' });
  if (response.status === 401 && path !== 'login') { csrf = ''; currentJob = null; $('panel').hidden = true; $('sign-in').hidden = false; $('enroll-card').hidden = true; }
  let result = {};
  try { result = await response.json(); } catch (_) { /* empty body */ }
  if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Запрос не выполнен');
  return result;
}
function el(tag, text, className) { const node = document.createElement(tag); if (text) node.textContent = text; if (className) node.className = className; return node; }
function action(text, run, className = '') {
  const button = el('button', text, 'text-button ' + className);
  button.addEventListener('click', async () => { button.disabled = true; try { await run(); } catch (error) { notice(error.message); } finally { button.disabled = false; } });
  return button;
}
function date(value) { return value ? new Date(value * 1000).toLocaleString('ru-RU', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' }) : '—'; }

function renderAccount(account) {
  const row = el('div', null, 'row admin-row'), detail = el('div', null, 'row-body'), actions = el('div', null, 'actions row-actions');
  const name = el('strong', account.label);
  if (account.page_seen_at) name.append(el('span', 'AA ID', 'badge'));
  if (account.pass_version) name.append(el('span', 'пропуск', 'badge'));
  const [stateText, tone] = STATE[account.state] || [account.state, ''];
  detail.append(name);
  const meta = el('span', null, 'meta');
  meta.append(el('span', account.group_name || (account.group_id ? 'Группа ' + account.group_id : 'без группы')), document.createTextNode(' · '), el('span', stateText, 'mark ' + tone));
  detail.append(meta);
  detail.append(el('span', `Проверена ${date(account.checked_at)} · ${account.automatic_renewal ? 'продлевается сама, последний раз ' + date(account.renewed_at) : 'без автопродления'} · ${account.page_seen_at ? 'на сайте ' + date(account.page_seen_at) : 'на сайт не входил'}`, 'meta'));
  actions.append(action('Группа', async () => { editingGroup = account; $('group-account').textContent = account.label; $('group-id').value = account.group_id || ''; $('group-name').value = account.group_name || ''; $('group-dialog').showModal(); }));
  actions.append(action('Имя', async () => { const label = prompt('Новое имя', account.label); if (label && label.trim()) { await api('accounts/' + account.id + '/label', 'PUT', { label: label.trim(), expected_version: account.version }); await refresh(); } }));
  actions.append(action('Проверить', async () => { await api('accounts/' + account.id + '/refresh', 'POST'); await refresh(); }));
  actions.append(action('Турникет', async () => {
    const data = await api('accounts/' + account.id + '/attendance');
    const time = s => new Date(s * 1000).toLocaleTimeString('ru-RU', { timeZone: 'Europe/Moscow', hour: '2-digit', minute: '2-digit' });
    const lines = (data.events || []).map(e => (e.direction === 'in' ? 'вход ' : 'выход ') + time(e.time)).join(', ');
    notice(`${account.label}: ${data.inside ? 'в вузе' : 'не в вузе'} · ${lines || 'сегодня проходов нет'}`);
  }));
  actions.append(action('Перевойти', () => startLogin(account.label, account.id)));
  actions.append(action('Завершить сессию', async () => { if (confirm('Завершить сессию МИРЭА для «' + account.label + '»? Аккаунт останется, но понадобится новый вход.')) { await api('accounts/' + account.id + '/end-session', 'POST'); await refresh(); } }, 'danger'));
  actions.append(action('Удалить', async () => { if (confirm('Удалить «' + account.label + '» и сохранённый пропуск с сервера?')) { await api('accounts/' + account.id, 'DELETE'); await refresh(); } }, 'danger'));
  row.append(detail, actions);
  return row;
}

function renderList() {
  const query = $('filter').value.trim().toLocaleLowerCase('ru');
  const shown = accounts.filter(a => !query || (a.label + ' ' + (a.group_name || '')).toLocaleLowerCase('ru').includes(query));
  $('account-count').textContent = String(accounts.length);
  $('empty-accounts').hidden = accounts.length > 0;
  $('accounts').replaceChildren(...shown.map(renderAccount));
}

async function refresh() {
  if (syncing) return;
  syncing = true;
  try {
    const data = await api('state'); csrf = data.csrf; accounts = data.accounts;
    $('sign-in').hidden = true; $('panel').hidden = false;
    // Don't rebuild the list under an open dialog or a pressed button.
    if (!document.querySelector('dialog[open]')) renderList();
    $('server-url').value = location.origin;
    $('pass-test-password').value = data.pass_testing.password;
    $('pass-test-expiry').textContent = 'Сменится ' + date(data.pass_testing.expires_at) + ' (полночь по Москве).';
    if (testingPasses) await previewTestPasses();
  } catch (error) { if ($('panel').hidden === false) notice(error.message); }
  finally { syncing = false; }
}

async function startLogin(label, account_id) {
  const result = await api('logins', 'POST', { label, account_id }); currentJob = result.id;
  $('enroll-title').textContent = label; $('owner-link').value = location.origin + result.enrollment_path;
  $('enroll-card').hidden = false; $('qr').hidden = true; $('qr-link').hidden = true;
  $('enroll-status').textContent = 'Готовим браузер для входа…';
  $('enroll-card').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  await pollLogin();
}
async function pollLogin() {
  const job = currentJob; if (!job) return;
  try {
    const result = await api('logins/' + job);
    if (job !== currentJob) return;
    $('enroll-status').textContent = result.error || ({ waiting: 'Ждём свободный браузер…', login: 'Ждём подтверждения от владельца…', finishing: 'Подтверждение получено, проверяем сессию…', complete: 'Аккаунт подключён.', expired: 'Время входа истекло, начните заново.', failed: 'Не удалось войти, попробуйте ещё раз.' }[result.status] || result.status);
    if (result.qr) { $('qr').src = '/panel/api/logins/' + job + '/qr'; $('qr').hidden = false; $('qr-link').href = result.qr; $('qr-link').hidden = false; }
    if (!result.qr) { $('qr').hidden = true; $('qr-link').hidden = true; }
    if (['complete', 'expired', 'failed', 'cancelled'].includes(result.status)) { currentJob = null; $('qr').hidden = true; $('qr-link').hidden = true; await refresh(); return; }
  } catch (error) { notice(error.message); }
  if (currentJob === job) setTimeout(pollLogin, 2000);
}
async function copy(input) {
  const element = $(input);
  try { if (navigator.clipboard) await navigator.clipboard.writeText(element.value); else { element.select(); if (!document.execCommand('copy')) throw Error(); } notice('Скопировано'); }
  catch (_) { element.focus(); element.select(); notice('Выделите и скопируйте текст.'); }
}
async function previewTestPasses() {
  const response = await fetch('/api/accounts', { headers: { 'X-Pass-Test-Key': $('pass-test-password').value }, cache: 'no-store' });
  if (!response.ok) throw Error('Не удалось проверить пропуска.');
  const result = await response.json();
  const count = (result.passes || []).filter(pass => pass.number).length;
  $('pass-test-status').textContent = `Для проверки доступно пропусков: ${count}. Разрешения владельцев не меняются.`;
}

$('group-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (!editingGroup) return;
  const raw = $('group-id').value.trim(), id = raw ? Number(raw) : null;
  if (id !== null && (!Number.isSafeInteger(id) || id <= 0)) { notice('Введите правильный ID группы.'); return; }
  try {
    await api('accounts/' + editingGroup.id + '/group', 'PUT', { group_id: id, group_name: id === null ? null : $('group-name').value.trim(), expected_version: editingGroup.version });
    $('group-dialog').close(); editingGroup = null; await refresh(); notice('Группа сохранена.');
  } catch (error) { notice(error.message); }
});
$('login-form').addEventListener('submit', async event => { event.preventDefault(); try { csrf = (await api('login', 'POST', { password: $('password').value })).csrf; $('password').value = ''; notice(''); await refresh(); } catch (error) { notice(error.message); } });
$('new-account').onclick = () => $('account-dialog').showModal();
$('account-form').addEventListener('submit', async event => { event.preventDefault(); try { await startLogin($('account-label').value); $('account-dialog').close(); $('account-form').reset(); } catch (error) { notice(error.message); } });
$('copy-server').onclick = () => copy('server-url');
$('copy-owner').onclick = () => copy('owner-link');
$('copy-pass-test').onclick = () => copy('pass-test-password');
$('filter').addEventListener('input', renderList);
$('close-enroll').onclick = async () => { if (currentJob) await api('logins/' + currentJob, 'DELETE').catch(error => notice(error.message)); currentJob = null; $('enroll-card').hidden = true; };
$('sign-out').onclick = async () => { testingPasses = false; $('pass-test-password').value = ''; $('pass-test-status').textContent = ''; try { await api('logout', 'POST'); $('panel').hidden = true; $('sign-in').hidden = false; $('enroll-card').hidden = true; currentJob = null; csrf = ''; } catch (error) { notice(error.message); } };
$('toggle-pass-test').onclick = async () => {
  testingPasses = !testingPasses;
  $('toggle-pass-test').setAttribute('aria-pressed', String(testingPasses));
  $('toggle-pass-test').textContent = testingPasses ? 'Скрыть' : 'Показать доступные пропуска';
  if (!testingPasses) { $('pass-test-status').textContent = ''; return; }
  try { await previewTestPasses(); } catch (error) { notice(error.message); }
};
document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => button.closest('dialog').close(); });
refresh();
setInterval(() => { if (!$('panel').hidden && !document.hidden) refresh(); }, 5000);
