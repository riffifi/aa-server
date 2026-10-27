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
  if (account.username) meta.append(el('span', '@' + account.username), document.createTextNode(' · '));
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
  actions.append(action('Журнал', async () => { $('audit-filter').value = account.id; showTab('audit'); }));
  actions.append(action('Перевойти', () => startLogin(account.label, account.id)));
  actions.append(action('Завершить сессию', async () => { if (confirm('Завершить сессию МИРЭА для «' + account.label + '»? Аккаунт останется, но понадобится новый вход.')) { await api('accounts/' + account.id + '/end-session', 'POST'); await refresh(); } }, 'danger'));
  actions.append(action('Удалить', async () => { if (confirm('Удалить «' + account.label + '» и сохранённый пропуск с сервера?')) { await api('accounts/' + account.id, 'DELETE'); await refresh(); } }, 'danger'));
  row.append(detail, actions);
  return row;
}

function renderList() {
  const query = $('filter').value.trim().toLocaleLowerCase('ru');
  const shown = accounts.filter(a => !query || (a.label + ' ' + (a.group_name || '') + ' @' + (a.username || '')).toLocaleLowerCase('ru').includes(query));
  $('account-count').textContent = String(accounts.length);
  $('empty-accounts').hidden = accounts.length > 0;
  $('accounts').replaceChildren(...shown.map(renderAccount));
}

async function refresh() {
  if (syncing) return;
  syncing = true;
  try {
    const data = await api('state'); csrf = data.csrf; accounts = data.accounts;
    if ($('panel').hidden) { $('sign-in').hidden = true; $('panel').hidden = false; showTab(currentTab()); }
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

// ---- Debug tabs -----------------------------------------------------------
const TABS = ['people', 'audit', 'logs', 'requests', 'tables', 'system'];
let tab = 'people', logs = [], lastLog = 0, tableName = 'accounts', tableOffset = 0;
const TABLE_PAGE = 100;
function currentTab() { const wanted = location.hash.slice(1); if (TABS.includes(wanted)) return wanted; try { return localStorage.getItem('panel-tab') || 'people'; } catch (_) { return 'people'; } }
function showTab(name) {
  tab = TABS.includes(name) ? name : 'people';
  document.querySelectorAll('[data-tab]').forEach(button => button.setAttribute('aria-selected', String(button.dataset.tab === tab)));
  TABS.forEach(id => { $('tab-' + id).hidden = id !== tab; });
  history.replaceState(null, '', '#' + tab);
  try { localStorage.setItem('panel-tab', tab); } catch (_) { /* private mode */ }
  loadTab().catch(error => notice(error.message));
}
async function loadTab() {
  if (tab === 'audit') await loadAudit();
  else if (tab === 'logs') await loadLogs();
  else if (tab === 'requests') await loadRequests();
  else if (tab === 'tables') await loadTables();
  else if (tab === 'system') await loadSystem();
}
function stamp(value) { return value ? new Date(value * 1000).toLocaleString('ru-RU', { timeZone: 'Europe/Moscow', day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '—'; }
function ago(value) {
  if (!value) return '—';
  const s = Math.max(0, Math.round(Date.now() / 1000 - value));
  return s < 60 ? s + ' с назад' : s < 3600 ? Math.round(s / 60) + ' мин назад' : s < 86400 ? Math.round(s / 3600) + ' ч назад' : Math.round(s / 86400) + ' дн назад';
}
function bytes(value) { return value == null ? '—' : value < 1024 ? value + ' Б' : value < 1048576 ? (value / 1024).toFixed(1) + ' КБ' : (value / 1048576).toFixed(1) + ' МБ'; }
// Fills a <table> from column titles and rows of cell values; `mark` can tag a row.
function grid(table, columns, rows, mark) {
  const head = el('tr'); columns.forEach(c => head.append(el('th', c)));
  const body = rows.map((cells, index) => {
    const tr = el('tr', null, mark ? mark(index) || '' : '');
    cells.forEach(value => { const text = value === null || value === undefined || value === '' ? '—' : String(value), td = el('td', text); if (value === null || value === undefined) td.className = 'null'; else if (text.length > 40) td.className = 'long'; tr.append(td); });
    return tr;
  });
  table.replaceChildren(el('thead'), el('tbody'));
  table.tHead.append(head); table.tBodies[0].append(...body);
  if (!rows.length) { const tr = el('tr'), td = el('td', 'Пусто', 'null'); td.colSpan = columns.length; tr.append(td); table.tBodies[0].append(tr); }
}
async function loadAudit() {
  const data = await api('debug/audit?q=' + encodeURIComponent($('audit-filter').value.trim()));
  grid($('audit-table'), ['Время', 'Действие', 'Кто', 'Аккаунт', 'ID аккаунта'],
    data.events.map(e => [stamp(e.created), e.action, e.actor, e.label, e.account_id]));
}
function logLine(r) {
  const line = el('div', null, 'log-line ' + r.level);
  line.append(el('span', stamp(r.time)), el('span', r.level.toUpperCase(), 'lvl'), el('span', r.logger + ': ' + r.message, 'msg'));
  if (r.detail) { const details = el('details'); details.append(el('summary', 'Трассировка', 'caption'), el('pre', r.detail)); line.append(details); }
  return line;
}
async function loadLogs(more = false) {
  if (!more) lastLog = 0;
  const query = 'debug/logs?level=' + $('log-level').value + '&q=' + encodeURIComponent($('log-filter').value.trim()) + (more ? '&after=' + lastLog : '');
  const data = await api(query);
  logs = more ? data.logs.concat(logs).slice(0, 1000) : data.logs;
  if (logs.length) lastLog = Math.max(lastLog, logs[0].id);
  $('logs').replaceChildren(...logs.map(logLine));
  $('logs-empty').hidden = logs.length > 0;
}
async function loadRequests() {
  const slow = Number($('requests-slow').value) || 0;
  const data = await api('debug/requests?q=' + encodeURIComponent($('requests-filter').value.trim()) + '&errors=' + $('requests-errors').checked + '&slow=' + slow);
  $('requests-since').textContent = 'С запуска ' + stamp(data.since) + '. Самые затратные маршруты сверху.';
  grid($('routes-table'), ['Маршрут', 'Запросов', 'Ошибок 5xx', 'Среднее, мс', 'Максимум, мс', 'Всего, мс', 'Последний'],
    data.routes.map(r => [r.route, r.count, r.errors, r.avg_ms, r.max_ms, r.total_ms, ago(r.last)]),
    i => data.routes[i].errors ? 'bad' : '');
  grid($('requests-table'), ['Время', 'Метод', 'Путь', 'Код', 'мс'],
    data.recent.map(r => [stamp(r.time), r.method, r.path, r.status, r.ms]),
    i => data.recent[i].status >= 500 ? 'bad' : data.recent[i].ms >= 1000 ? 'slow' : '');
}
async function loadTables() {
  const list = await api('debug/tables');
  $('table-list').replaceChildren(...list.tables.map(t => {
    const button = el('button', t.name + ' · ' + t.rows); button.setAttribute('aria-pressed', String(t.name === tableName));
    button.onclick = () => { tableName = t.name; tableOffset = 0; loadTables().catch(error => notice(error.message)); };
    return button;
  }));
  const data = await api('debug/tables/' + encodeURIComponent(tableName) + '?offset=' + tableOffset + '&limit=' + TABLE_PAGE + '&q=' + encodeURIComponent($('table-filter').value.trim()));
  const times = new Set(['created', 'expires', 'expires_at', 'checked_at', 'updated_at', 'page_seen_at']);
  grid($('table-rows'), data.columns, data.rows.map(row => row.map((v, i) => times.has(data.columns[i]) && typeof v === 'number' ? stamp(v) : v)));
  $('table-info').textContent = `${data.name}: строки ${data.total ? tableOffset + 1 : 0}–${tableOffset + data.rows.length} из ${data.total}, новые сверху`;
  $('table-prev').disabled = tableOffset === 0;
  $('table-next').disabled = tableOffset + data.rows.length >= data.total;
}
function fact(label, value) { const box = el('div'); box.append(el('span', label), el('span', value === null || value === undefined ? '—' : String(value))); return box; }
async function loadSystem() {
  const s = await api('debug/system');
  const p = s.process, up = Math.round(s.now - s.started);
  $('system-facts').replaceChildren(
    fact('Работает', `${Math.floor(up / 86400)} дн ${Math.floor(up % 86400 / 3600)} ч ${Math.floor(up % 3600 / 60)} мин`),
    fact('Запущен', stamp(s.started)),
    fact('Память процесса', p.memory_kb ? (p.memory_kb / 1024).toFixed(0) + ' МБ' : '—'),
    fact('Нагрузка (1/5/15 мин)', p.load ? p.load.map(v => v.toFixed(2)).join(' / ') : '—'),
    fact('Python / PID', p.python + ' / ' + p.pid),
    fact('База', bytes(s.files['sessions.sqlite3']) + ' + WAL ' + bytes(s.files['sessions.sqlite3-wal'])),
    fact('Проверка сессий', { running: 'работает', stopped: 'остановлена', off: 'выключена' }[s.background.refresh_loop]),
    fact('Браузер входа', s.browser.running ? 'запущен' : 'не запущен'),
    fact('Свободно слотов входа / продления', s.browser.login_slots_free + ' / ' + s.browser.renew_slots_free),
    fact('Статус «внутри»', `${s.presence.cached} в кэше, ${s.presence.checking} проверяются, хранится ${s.presence.ttl} с`),
    fact('AA ID', s.aaid ? `${s.aaid.enforce ? 'обязателен' : 'необязателен'}, ключи: ${s.aaid.keys.join(', ') || 'нет'}` : 'не настроен'),
    fact('Ключи AA ID обновлены', s.aaid ? ago(s.aaid.keys_fetched) : '—'),
    fact('Telegram', s.telegram ? `@${s.telegram.username || '…'}, ${s.telegram.task === 'running' ? 'работает' : 'остановлен'}` : 'не настроен'),
  );
  grid($('calendars-table'), ['Группа', 'ID', 'Скачано', 'Последняя ошибка', 'Размер'],
    s.calendars.map(c => [c.name, c.group_id, ago(c.fetched), c.failed ? ago(c.failed) : null, bytes(c.size)]),
    i => s.calendars[i].failed ? 'bad' : '');
  grid($('jobs-table'), ['ID', 'Имя', 'Статус', 'Возраст', 'Ошибка', 'Диагностика'],
    s.browser.jobs.map(j => [j.id, j.label, j.status, ago(Date.now() / 1000 - j.age), j.error, j.diag]),
    i => s.browser.jobs[i].status === 'failed' ? 'bad' : '');
}
document.querySelectorAll('[data-tab]').forEach(button => { button.onclick = () => showTab(button.dataset.tab); });
let typing = null;
function later(run) { clearTimeout(typing); typing = setTimeout(() => run().catch(error => notice(error.message)), 300); }
$('audit-filter').addEventListener('input', () => later(loadAudit));
$('audit-reload').onclick = () => loadAudit().catch(error => notice(error.message));
$('log-level').onchange = () => loadLogs().catch(error => notice(error.message));
$('log-filter').addEventListener('input', () => later(loadLogs));
['requests-filter', 'requests-slow'].forEach(id => $(id).addEventListener('input', () => later(loadRequests)));
$('requests-errors').onchange = () => loadRequests().catch(error => notice(error.message));
$('table-filter').addEventListener('input', () => { tableOffset = 0; later(loadTables); });
$('table-prev').onclick = () => { tableOffset = Math.max(0, tableOffset - TABLE_PAGE); loadTables().catch(error => notice(error.message)); };
$('table-next').onclick = () => { tableOffset += TABLE_PAGE; loadTables().catch(error => notice(error.message)); };

refresh();
setInterval(() => { if (!$('panel').hidden && !document.hidden && tab === 'people') refresh(); }, 5000);
// Live tabs: logs follow new records, requests and system refresh themselves.
setInterval(() => {
  if ($('panel').hidden || document.hidden || document.querySelector('dialog[open]')) return;
  if (tab === 'logs' && $('log-follow').checked) loadLogs(true).catch(() => {});
  else if (tab === 'requests' || tab === 'system') loadTab().catch(() => {});
}, 5000);
