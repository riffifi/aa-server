'use strict';
const $ = id => document.getElementById(id);
let csrf = '', currentJob = null, accounts = [], syncing = false, editingGroup = null;
const STATE = { blocked:['Аккаунт заморожен','bad'], valid: ['Сессия работает', 'ok'], expired: ['Сессия истекла', 'bad'], forbidden: ['Доступ запрещён', 'bad'], unavailable: ['Пульс недоступен', 'wait'], invalidResponse: ['Непонятный ответ Пульса', 'wait'] };

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
  const renewal = account.renewal || {};
  const statistics = renewal.statistics || {};
  const renewalText = renewal.status === 'requires_login' ? 'SSO требует новый QR-вход; повторные попытки остановлены' :
    renewal.status === 'failed' ? 'не удалось продлить, попытка ' + date(renewal.last_attempt) :
    renewal.status === 'running' ? (Date.now() / 1000 - (renewal.last_attempt || 0) > 120 ? 'прошлая попытка не завершена' : 'идёт продление') : account.automatic_renewal ? 'автопродление настроено; последний успех ' + date(renewal.last_success) : 'без автопродления';
  detail.append(el('span', `Проверена ${date(account.checked_at)} · ${renewalText} · ${account.page_seen_at ? 'на сайте ' + date(account.page_seen_at) : 'на сайт не входил'}`, 'meta'));
  if (statistics.attempts) detail.append(el('span', `Авто: ${statistics.succeeded || 0} из ${statistics.attempts}; восстановлено ${statistics.recovered || 0}; срок вырос ${statistics.expiry_extended || 0}`, 'meta'));
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
$('filter').addEventListener('input', renderList);
$('close-enroll').onclick = async () => { if (currentJob) await api('logins/' + currentJob, 'DELETE').catch(error => notice(error.message)); currentJob = null; $('enroll-card').hidden = true; };
$('sign-out').onclick = async () => { try { await api('logout', 'POST'); $('panel').hidden = true; $('sign-in').hidden = false; $('enroll-card').hidden = true; currentJob = null; csrf = ''; } catch (error) { notice(error.message); } };
document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => button.closest('dialog').close(); });

// ---- Debug tabs -----------------------------------------------------------
const TABS = ['people', 'audit', 'usage', 'bans', 'logs', 'requests', 'tables', 'system'];
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
	if(tab==='usage') {usageBefore=0;await loadUsage();return;}
	if(tab==='bans') {await loadBans();return;}
  if (tab === 'audit') await loadAudit();
  else if (tab === 'logs') await loadLogs();
  else if (tab === 'requests') await loadRequests();
  else if (tab === 'tables') await loadTables();
  else if (tab === 'system') await loadSystem();
}
let usageBefore=0,usageNext=0,usageSequence=0;
async function loadUsage(){
 const sequence=++usageSequence;
 const q=new URLSearchParams({before:String(usageBefore),mirea_id:$('usage-person').value.trim(),kind:$('usage-kind').value});
 const data=await api('usage?'+q);if(sequence!==usageSequence)return;
 usageNext=data.next_before;$('usage-next').hidden=!usageNext;
 $('usage-count').textContent=`Показано событий: ${data.events.length}. За 24 часа: ${(data.summary_24h||[]).map(s=>s.kind+': '+s.count).join('; ')||'нет событий'}. Время — Москва.`;
 grid($('usage-table'),['Обнаружено','Действие','Кто (ID МИРЭА)','Чей аккаунт (ID МИРЭА)','Аккаунт','Время пары / события','Пара','Результат / переход'],data.events.map(e=>{
  const d=e.details||{};
  const transition=d.new_state===undefined?'':`${d.previous_state??'неизвестно'} → ${d.new_state}`;
  return [stamp(e.created),e.kind,(e.actor_mirea ? ((e.actor_label || '') + ' · ' + e.actor_mirea) : null)||'не установлен',e.target_mirea||'—',e.target_label||e.account_id||'—',d.event_time?stamp(d.event_time):d.lesson_start?`${stamp(d.lesson_start)} — ${stamp(d.lesson_end)}`:'—',d.subject||'—',transition||d.result||d.channel||(d.blocked===undefined?'':d.blocked?'заблокирован':'разблокирован')];
 }));
}
async function loadBans(){const data=await api('bans');grid($('ban-table'),['ID МИРЭА','Статус','Причина','Изменено'],data.bans.map(b=>[b.pulse_uid,b.blocked?'Заблокирован':'Разблокирован',b.reason,stamp(b.updated_at)]));}
async function changeBan(blocked){
 const id=$('ban-mirea').value.trim();if(!id){notice('Введите ID МИРЭА');return;}
 await api('bans','PUT',{mirea_id:id,blocked,reason:$('ban-reason').value});await loadBans();notice(blocked?'Аккаунт заблокирован':'Аккаунт разблокирован; требуется новый вход');
}
$('usage-reload').onclick=()=>{usageBefore=0;loadUsage().catch(e=>notice(e.message));};
$('usage-next').onclick=()=>{usageBefore=usageNext;loadUsage().catch(e=>notice(e.message));};
$('ban-form').onsubmit=event=>{event.preventDefault();changeBan(true).catch(e=>notice(e.message));};
$('ban-unblock').onclick=()=>changeBan(false).catch(e=>notice(e.message));
function stamp(value) { return value ? new Date(value * 1000).toLocaleString('ru-RU', { timeZone: 'Europe/Moscow', year: 'numeric', day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '—'; }
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
async function loadRuntimeSystem() {
  const s = await api('debug/system');
  fillRecordAccounts();
  const p = s.process, up = Math.round(s.now - s.started);
  $('system-facts').replaceChildren(
    fact('Работает', `${Math.floor(up / 86400)} дн ${Math.floor(up % 86400 / 3600)} ч ${Math.floor(up % 3600 / 60)} мин`),
    fact('Запущен', stamp(s.started)),
    fact('Память процесса', p.memory_kb ? (p.memory_kb / 1024).toFixed(0) + ' МБ' : '—'),
    fact('Нагрузка (1/5/15 мин)', p.load ? p.load.map(v => v.toFixed(2)).join(' / ') : '—'),
    fact('Go / PID', p.go + ' / ' + p.pid),
    fact('PostgreSQL', bytes(s.files.postgresql_bytes)),
    fact('Проверка сессий', { running: 'работает', stopped: 'остановлена', off: 'выключена' }[s.background.refresh_loop]),
    fact('Браузер входа', s.browser.running ? 'запущен' : 'не запущен'),
    fact('Свободно слотов входа / продления', s.browser.login_slots_free + ' / ' + s.browser.renew_slots_free),
    fact('Статус «внутри»', `${s.presence.cached} в кэше, ${s.presence.checking} проверяются, хранится ${s.presence.ttl} с`),
    fact('AA ID', s.aaid ? `${s.aaid.enforce ? 'обязателен' : 'необязателен'}, ключи: ${s.aaid.keys.join(', ') || 'нет'}` : 'не настроен'),
    fact('Ключи AA ID обновлены', s.aaid ? ago(s.aaid.keys_fetched) : '—'),
    fact('Telegram', s.telegram ? `@${s.telegram.username || '…'}, ${s.telegram.task === 'running' ? 'работает' : 'остановлен'}` : 'не настроен'),
  );
  grid($('calendars-table'), ['Группа', 'ID', 'Скачано', 'Последняя ошибка', 'Размер', 'Журнал МИРЭА'],
    s.calendars.map(c => [c.name, c.group_id, ago(c.fetched), c.failed ? ago(c.failed) : null, bytes(c.size),
      !c.journal ? null : c.journal.error ? c.journal.error + ', ' + ago(c.journal.fetched) : `${c.journal.lessons} пар, ${c.journal.statuses} отметок, ${ago(c.journal.fetched)}`]),
    i => s.calendars[i].failed ? 'bad' : '');
  grid($('jobs-table'), ['ID', 'Имя', 'Статус', 'Возраст', 'Ошибка', 'Диагностика'],
    (s.browser.jobs || []).map(j => [j.id, j.label, j.status, ago(Date.now() / 1000 - j.age), j.error, j.diag]),
    i => (s.browser.jobs || [])[i].status === 'failed' ? 'bad' : '');
  const traffic = s.browser.traffic || {}, assets = s.browser.asset_cache || {};
  $('browser-network-status').textContent = `С момента запуска сервиса. Заблокировано ресурсов интерфейса Pulse: ${traffic.blocked_auth_resources || 0}. Прямые запросы браузера к API: ${traffic.api_requests || 0}, получено ${bytes(traffic.api_decoded_bytes)}. Загрузки статических файлов: ${assets.fetches || 0}, Content-Length ${bytes(assets.content_length_bytes)}. Явная запись диагностики может загружать интерфейс Pulse.`;
  grid($('browser-network-table'), ['Источник / тип', 'Запросы', 'Получено', 'Ошибки'],
    Object.entries(traffic.page_network || {}).map(([key, value]) => [key, value.requests, bytes(value.encoded_response_bytes), value.failures]));
}
async function loadSystem() {
  // Runtime diagnostics and resource collection fail independently.
  const results = await Promise.allSettled([loadRuntimeSystem(), loadMetrics(), loadRenewalStatistics()]);
  results.filter(result => result.status === 'rejected').forEach(result => notice(result.reason.message));
}

const RENEWAL_RESULT = { running: 'Выполняется', succeeded: 'Успех', failed: 'Ошибка', requires_login: 'Нужен QR', interrupted: 'Прервана' };
let renewalSequence = 0;
async function loadRenewalStatistics() {
  const sequence = ++renewalSequence;
  const selected = $('renewal-account').value;
  const data = await api('state' + (selected ? '?renewal_history=' + encodeURIComponent(selected) : ''));
  if (sequence !== renewalSequence || $('renewal-account').value !== selected) return;
  accounts = data.accounts; csrf = data.csrf;
  const totals = {}, keys = ['cookie_refreshes', 'attempts', 'succeeded', 'failed', 'requires_login', 'interrupted', 'recovered', 'confirmed_benefit', 'expiry_extended', 'expiry_unchanged', 'expiry_unknown'];
  keys.forEach(key => { totals[key] = accounts.reduce((sum, a) => sum + (Number(((a.renewal || {}).statistics || {})[key]) || 0), 0); });
  const since = accounts.map(a => ((a.renewal || {}).statistics || {}).since).filter(value => value > 0);
  $('renewal-statistics-status').textContent = since.length ? `Счётчики с ${stamp(since.reduce((first, value) => Math.min(first, value), Infinity))}; сохраняются при перезапуске. Учитываются аккаунты, которые есть на сервере.` : 'После обновления ещё не было попыток автопродления. Старые попытки и обычные входы не засчитываются задним числом.';
  const completed = totals.succeeded + totals.failed + totals.requires_login + totals.interrupted;
  $('renewal-facts').replaceChildren(
    fact('Обновления cookie из ответов МИРЭА', totals.cookie_refreshes),
    fact('Попыток', totals.attempts), fact('Успешно', totals.succeeded),
    fact('Успех среди завершённых', completed ? (100 * totals.succeeded / completed).toFixed(1) + '%' : '—'),
    fact('С подтверждённым эффектом', totals.confirmed_benefit),
    fact('Аккаунтов с эффектом', accounts.filter(a => (((a.renewal || {}).statistics || {}).confirmed_benefit || 0) > 0).length),
    fact('Сейчас нужен новый QR', accounts.filter(a => (a.renewal || {}).status === 'requires_login').length),
    fact('Восстановлено истёкших', totals.recovered), fact('Срок cookie вырос', totals.expiry_extended),
    fact('Срок не вырос', totals.expiry_unchanged), fact('Срок неизвестен', totals.expiry_unknown),
    fact('Ошибки', totals.failed), fact('Требуется QR', totals.requires_login), fact('Прерваны', totals.interrupted),
    fact('Не завершены', Math.max(0, totals.attempts - completed)));
  grid($('renewal-table'), ['Аккаунт', 'Cookie МИРЭА', 'Последняя cookie', 'Попытки SSO', 'Успехи SSO', 'Восстановлено', 'Срок вырос', 'Ошибки / QR', 'Последний успех SSO', 'Последняя попытка SSO'], accounts.map(a => {
    const r = a.renewal || {}, s = r.statistics || {};
    return [a.label, s.cookie_refreshes || 0, stamp(s.last_cookie_refresh), s.attempts || 0, s.succeeded || 0, s.recovered || 0, s.expiry_extended || 0, `${s.failed || 0} / ${s.requires_login || 0}`, stamp(r.last_success), stamp(r.last_attempt)];
  }));
  const placeholder = el('option', 'Выберите аккаунт'); placeholder.value = '';
  $('renewal-account').replaceChildren(placeholder, ...accounts.map(a => { const option = el('option', a.label); option.value = a.id; return option; }));
  if (accounts.some(a => a.id === selected)) $('renewal-account').value = selected;
  const account = accounts.find(a => a.id === $('renewal-account').value);
  const statistics = ((account || {}).renewal || {}).statistics || {};
  const history = [...(statistics.history || [])].reverse();
  if (statistics.active) history.unshift(statistics.active);
  grid($('renewal-history'), ['Начало', 'Результат', 'До: срок cookie', 'После: срок cookie', 'Изменение срока', 'Длительность', 'Была истекшей'], history.map(h => [stamp(h.started), RENEWAL_RESULT[h.result] || h.result, stamp(h.before_expiry), stamp(h.after_expiry), h.expiry_delta_seconds == null ? 'неизвестно' : `${h.expiry_delta_seconds > 0 ? '+' : ''}${Math.round(h.expiry_delta_seconds)} с`, h.finished ? (h.duration_ms / 1000).toFixed(1) + ' с' : '—', h.previously_expired ? 'да' : 'нет']));
  fillRecordAccounts();
}
$('renewal-account').addEventListener('change', () => loadRenewalStatistics().catch(error => notice(error.message)));

function metricChart(id, history, keys, scale = 1) {
  const canvas = $(id), ctx = canvas.getContext('2d');
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  history = (history || []).filter(row => Number.isFinite(row.time));
  if (!history.length) { ctx.fillStyle = '#777'; ctx.font = '12px sans-serif'; ctx.fillText('Нет измерений', 8, 24); return; }
  const max = Math.max(1, ...history.flatMap(row => keys.map(key => (row[key] || 0) / scale)));
  const first = history[0].time, span = Math.max(1, history[history.length - 1].time - first);
  keys.forEach((key, index) => {
    ctx.strokeStyle = ['#4778df', '#d78b24'][index]; ctx.lineWidth = 2; ctx.beginPath();
    let connected = false;
    history.forEach(row => {
      if (row[key] == null) { connected = false; return; }
      const x = (row.time - first) / span * canvas.width, y = canvas.height - 6 - row[key] / scale / max * (canvas.height - 22);
      if (connected) ctx.lineTo(x, y); else ctx.moveTo(x, y);
      ctx.fillStyle = ctx.strokeStyle; ctx.fillRect(x - 1, y - 1, 3, 3);
      connected = true;
    });
    ctx.stroke();
  });
  ctx.fillStyle = '#777'; ctx.font = '12px sans-serif'; ctx.fillText(max.toFixed(1), 4, 12);
  ctx.fillText(new Date(first * 1000).toLocaleTimeString(), 4, canvas.height - 2);
}
async function loadMetrics() {
  const m = await api('debug/metrics');
  const missing = { missing: 'Файл статистики ещё не создан. Проверьте службу сборщика; установщик: --metrics.', permission: 'Сервер не может прочитать статистику. Проверьте права каталога сборщика.', invalid: 'Повреждён файл статистики. Проверьте журнал службы сборщика.', io: 'Не удалось прочитать статистику. Проверьте службу сборщика и диск.' };
  $('metrics-status').textContent = !m.available ? missing[m.reason_code] || 'Сборщик статистики недоступен. Проверьте службу; установщик: --metrics (Linux/systemd).' :
    `${m.stale ? 'Данные устарели. Проверьте службу сборщика. ' : ''}Обновлено: ${stamp(m.collected_at)}. Счётчики с ${stamp(m.since)}. Ошибок опроса: ${m.errors}.`;
  if (!m.available) {
    $('metrics-facts').replaceChildren(); $('metrics-table').replaceChildren();
    ['metrics-cpu', 'metrics-memory', 'metrics-network'].forEach(id => metricChart(id, [], []));
    return;
  }
  const t = m.total;
  $('metrics-facts').replaceChildren(
    fact('CPU', t.cpu_percent == null ? '—' : t.cpu_percent.toFixed(1) + '%'),
    fact('ОЗУ всех сервисов', bytes(t.memory_bytes)), fact('Процессы и потоки', t.processes),
    fact('Получено всего', bytes(t.rx_bytes)), fact('Отправлено всего', bytes(t.tx_bytes)),
    fact('Диск: прочитано / записано', bytes(t.disk_read_bytes) + ' / ' + bytes(t.disk_write_bytes)),
    fact('Свободно на диске сборщика', bytes(m.filesystem.available_bytes)));
  grid($('metrics-table'), ['Сервис', 'Состояние', 'CPU', 'ОЗУ', 'Приём/с', 'Отправка/с', 'Получено всего', 'Отправлено всего', 'Диск: чтение / запись'],
    m.components.map(c => [c.service, c.unavailable ? 'нет данных' : c.running ? 'работает' : 'остановлен', c.cpu_percent == null ? null : c.cpu_percent.toFixed(1) + '%', bytes(c.memory_bytes), bytes(c.rx_bytes_per_second), bytes(c.tx_bytes_per_second), bytes(c.rx_bytes), bytes(c.tx_bytes), bytes(c.disk_read_bytes) + ' / ' + bytes(c.disk_write_bytes)]));
  const history = [...(m.history || []).filter(point => point.time < m.collected_at), { ...t, time: m.collected_at }];
  metricChart('metrics-cpu', history, ['cpu_percent']);
  metricChart('metrics-memory', history, ['memory_bytes'], 1048576);
  metricChart('metrics-network', history, ['rx_bytes_per_second', 'tx_bytes_per_second'], 1024);
}
function fillRecordAccounts() {
  const select = $('record-account'), keep = select.value;
  const usable = accounts.filter(a => a.automatic_renewal && a.state === 'valid');
  select.replaceChildren(...usable.map(a => { const option = el('option', a.label + (a.group_name ? ' · ' + a.group_name : '')); option.value = a.id; return option; }));
  if (usable.some(a => a.id === keep)) select.value = keep;
  $('record-start').disabled = !usable.length;
  if (!usable.length) $('record-status').textContent = 'Нет аккаунтов с рабочей сессией и автопродлением.';
}
$('record-start').onclick = async () => {
  const button = $('record-start'); button.disabled = true;
  $('record-status').textContent = 'Открываем Пульс и записываем запросы…'; $('record-output').hidden = true; $('record-copy').hidden = true;
  try {
    const report = await api('debug/pulse-record', 'POST', { account_id: $('record-account').value });
    $('record-output').textContent = JSON.stringify(report, null, 1);
    $('record-output').hidden = false; $('record-copy').hidden = false;
    $('record-status').textContent = `Готово: страниц ${report.visited.length}, запросов ${report.calls.length}, методов в коде ${report.methods.length}.`;
  } catch (error) { $('record-status').textContent = error.message; }
  finally { button.disabled = false; }
};
$('record-copy').onclick = async () => {
  try { await navigator.clipboard.writeText($('record-output').textContent); notice('Отчёт скопирован'); }
  catch (_) { notice('Выделите текст отчёта и скопируйте вручную.'); }
};
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
