'use strict';
const $ = id => document.getElementById(id);
let job = null;
// Sign-in for the AntiAttendance site: ?continue=aa&state=…, with fresh=1 asking for a
// new MIREA QR sign-in so the site gets a working MIREA session again.
const params = new URLSearchParams(location.search);
const handoff = params.get('continue') === 'aa' && /^[A-Za-z0-9_-]{16,128}$/.test(params.get('state') || '') ? params.get('state') : null;
let needFresh = Boolean(handoff) && params.get('fresh') === '1';

const STATUS = {
  waiting: 'Готовим безопасный вход…',
  login: 'Отсканируйте QR в приложении МИРЭА или откройте ссылку ниже.',
  finishing: 'Подтверждение получено. Завершаем вход…',
  complete: 'Готово! Открываем ваш AA ID…',
  expired: 'Время входа истекло. Начните заново.',
  failed: 'Не удалось войти. Начните заново.',
  cancelled: 'Вход отменён.'
};

function el(tag, text, className) { const n = document.createElement(tag); if (text) n.textContent = text; if (className) n.className = className; return n; }
function when(seconds) { return seconds ? new Date(seconds * 1000).toLocaleString('ru-RU') : '—'; }
function showError(message) { $('error').textContent = message || ''; $('error').hidden = !message; }
async function api(path, options = {}) {
  const response = await fetch(path, { cache: 'no-store', credentials: 'same-origin', ...options });
  let data = {};
  try { data = await response.json(); } catch (_) { /* empty body */ }
  if (!response.ok) { const error = Error(typeof data.detail === 'string' ? data.detail : 'Сервер AA ID недоступен.'); error.status = response.status; throw error; }
  return data;
}

async function load() {
  showError('');
  try {
    const me = await api('web/me');
    if (needFresh) {
      $('account').hidden = true; $('handoff').hidden = true; $('signin').hidden = false; $('skip-fresh').hidden = false;
      $('signin-lead').textContent = 'Сервер AntiAttendance пока не видит вашу сессию МИРЭА, поэтому на паре вас не видно и отметить вас нельзя. Войдите по QR ещё раз, чтобы подключить её.';
      return;
    }
    render(me);
  } catch (error) {
    $('account').hidden = true; $('signin').hidden = false;
    $('who').textContent = 'Один аккаунт на человека'; $('site-link').hidden = true;
    if (error.status !== 401) showError(error.message);
  }
}

async function continueToSite() {
  $('continue').disabled = true;
  try {
    const result = await api('web/authorize', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ state: handoff }) });
    location.assign(result.redirect);
  } catch (error) { showError(error.message); $('continue').disabled = false; }
}

let account = null;
async function loadSettings() {
  try {
    account = await api('web/account');
    $('settings').hidden = !account.account; $('settings-note').hidden = true;
    $('name').value = account.label === 'Без имени' ? '' : account.label;
    $('username').value = (lastMe && lastMe.username) || '';
    showWho();
    if (account.account) await loadPass();
    if (location.hash === '#settings') $('settings').scrollIntoView({ block: 'start' });
  } catch (error) {
    $('settings').hidden = true; $('pass').hidden = true; $('settings-note').textContent = error.message; $('settings-note').hidden = false;
  }
}

function showWho() {
  const name = account && account.label && account.label !== 'Без имени' ? account.label : null;
  const parts = [lastMe && lastMe.username ? '@' + lastMe.username : null, name, lastMe && lastMe.group_name].filter(Boolean);
  $('who').textContent = parts.length ? parts.join(' · ') : 'Вы вошли · AA ID';
}

// Pass: the people allowed to use it, and one switch that blocks everyone, them included.
let pass = null;
async function loadPass() {
  try { renderPass(await api('web/pass')); } catch (error) { $('pass').hidden = true; showError(error.message); }
}

function personLine(person, extra) {
  const row = el('div', null, 'row'), body = el('div', null, 'row-body');
  body.append(el('strong', person.label || (person.username ? '@' + person.username : 'Без имени')));
  const meta = person.kind === 'telegram' ? 'разрешено в Telegram' : [person.username ? '@' + person.username : null, person.group_name].filter(Boolean).join(' · ');
  if (meta) body.append(el('span', meta, 'meta'));
  row.append(body, extra);
  return row;
}

function renderPass(data) {
  pass = data;
  $('pass').hidden = false;
  $('pass-state').textContent = data.pass_allowed
    ? 'Пропуск могут использовать только люди из списка.'
    : 'Пропуск запрещён всем, даже людям из списка.';
  $('pass-toggle').textContent = data.pass_allowed ? 'Запретить пропуск всем' : 'Разрешить людям из списка';
  $('pass-toggle').className = data.pass_allowed ? 'secondary danger' : 'primary';
  const requests = data.requests || [];
  $('requests-block').hidden = requests.length === 0;
  $('pass-requests').replaceChildren(...requests.map(person => {
    const actions = el('div', null, 'actions');
    const allow = el('button', 'Разрешить', 'primary'), deny = el('button', 'Запретить', 'secondary danger');
    allow.type = deny.type = 'button';
    allow.addEventListener('click', () => {
      const lines = ['Разрешить «' + who(person) + '» использовать ваш пропуск.'];
      if (!data.pass_allowed) lines.push('Сейчас пропуск запрещён всем: разрешение заработает, когда вы снова разрешите пропуск.');
      passChange(lines, () => api('web/pass/requests/' + encodeURIComponent(person.id) + '/allow', { method: 'POST' }), allow);
    });
    deny.addEventListener('click', () => passChange(['Отклонить запрос «' + who(person) + '». Повторить его можно будет через сутки.'],
      () => api('web/pass/requests/' + encodeURIComponent(person.id) + '/deny', { method: 'POST' }), deny));
    actions.append(allow, deny);
    return personLine(person, actions);
  }));
  $('pass-empty').hidden = data.people.length > 0;
  $('pass-people').replaceChildren(...data.people.map(person => {
    const button = el('button', 'Убрать', 'text-button danger');
    button.type = 'button';
    button.addEventListener('click', () => passChange(['Убрать «' + who(person) + '» из списка. Этот человек больше не сможет использовать ваш пропуск.'],
      () => api('web/pass/people/' + person.kind + '/' + encodeURIComponent(person.id), { method: 'DELETE' }), button));
    return personLine(person, button);
  }));
}

function who(person) { return person.label && person.username ? person.label + ' (@' + person.username + ')' : person.label || '@' + person.username; }

async function passChange(lines, run, button) {
  if (!await confirmChanges(lines)) return false;
  if (button) button.disabled = true;
  try { renderPass(await run()); $('pass-status').textContent = 'Сохранено'; return true; }
  catch (error) { $('pass-status').textContent = error.message; return false; }
  finally { if (button) button.disabled = false; }
}

async function searchPeople(event) {
  event.preventDefault();
  const query = $('people-q').value.trim();
  if (query.replace(/^@/, '').length < 2) { $('pass-status').textContent = 'Введите хотя бы 2 символа'; return; }
  try {
    const data = await api('web/people?q=' + encodeURIComponent(query));
    $('pass-status').textContent = data.people.length ? '' : 'Никого не нашли';
    const listed = new Set(pass ? pass.people.filter(p => p.kind === 'aaid').map(p => p.account_id) : []);
    $('people-results').replaceChildren(...data.people.map(person => {
      const button = el('button', listed.has(person.account_id) ? 'Уже в списке' : 'Разрешить', 'text-button');
      button.type = 'button'; button.disabled = listed.has(person.account_id);
      button.addEventListener('click', async () => {
        const lines = ['Разрешить «' + who(person) + '» использовать ваш пропуск.'];
        if (pass && !pass.pass_allowed) lines.push('Сейчас пропуск запрещён всем: разрешение заработает, когда вы снова разрешите пропуск.');
        if (await passChange(lines, () => api('web/pass/people', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ account_id: person.account_id }) }), button)) {
          button.textContent = 'Уже в списке'; button.disabled = true;
        }
      });
      return personLine(person, button);
    }));
  } catch (error) { $('pass-status').textContent = error.message; }
}

let lastMe = null;
function render(me) {
  const first = $('account').hidden;
  lastMe = me;
  $('signin').hidden = true; $('account').hidden = false;
  showWho();
  if (me.site_url) { $('site-link').href = me.site_url; $('site-link').hidden = false; }
  if (first) loadSettings();
  // Signed in already: go straight back to AntiAttendance, no extra click.
  if (handoff && first) { continueToSite(); }
  if (handoff) {
    $('handoff').hidden = false;
    $('handoff-lead').textContent = 'Сайт AntiAttendance просит войти как ' + (me.group_name || 'вы') + '. Он узнает ваш AA ID и группу.';
  }
  if (first) { loadSchedule(); loadAttendance(); }
  $('group').textContent = me.group_name || (me.group_id ? 'Группа ' + me.group_id : 'не определена');
  $('created').textContent = when(me.created);
  $('user-id').textContent = me.id;
  $('device-note').textContent = `В приложении можно войти не больше чем на ${me.max_sessions} устройствах; вход на новом выводит самое давнее. На сайте одновременно открыт один браузер.`;
  $('devices').replaceChildren(...me.sessions.map(device => {
    const row = el('div', null, 'row'), body = el('div', null, 'row-body');
    const name = el('strong', device.device);
    if (device.current) name.append(el('span', 'это окно', 'badge'));
    body.append(name);
    body.append(el('div', (device.kind === 'web' ? 'Сайт' : 'Приложение') + ' · активность ' + when(device.last_used), 'meta'));
    row.append(body);
    if (!device.current) {
      const button = el('button', 'Отключить', 'danger');
      button.addEventListener('click', async () => {
        button.disabled = true;
        try { await api('web/sessions/' + encodeURIComponent(device.id), { method: 'DELETE' }); await load(); }
        catch (error) { showError(error.message); button.disabled = false; }
      });
      const actions = el('div', null, 'actions'); actions.append(button); row.append(actions);
    }
    return row;
  }));
}

function zoned(seconds) { return new Date(seconds * 1000).toLocaleString('ru-RU', { timeZone: 'Europe/Moscow', day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' }); }

async function loadSchedule() {
  try {
    const data = await api('web/schedule');
    $('schedule-link').href = data.url;
    const lesson = data.current || data.next;
    if (!data.group_id) { $('lesson-title').textContent = 'Группа не определена'; $('lesson-meta').textContent = ''; return; }
    if (!lesson) { $('lesson-title').textContent = 'Сегодня пар больше нет'; $('lesson-meta').textContent = ''; return; }
    $('lesson-title').textContent = (data.current ? 'Сейчас: ' : 'Далее: ') + lesson.subject;
    $('lesson-meta').textContent = [lesson.start + '–' + lesson.end, lesson.type, lesson.location].filter(Boolean).join(' · ');
  } catch (error) { $('lesson-title').textContent = error.message; $('lesson-meta').textContent = ''; }
}

async function loadAttendance() {
  $('acs-note').hidden = true;
  $('acs-refresh').disabled = true;
  try {
    const data = await api('web/attendance');
    $('last-in').textContent = data.last_in ? zoned(data.last_in.time) : '—';
    $('last-out').textContent = data.last_out ? zoned(data.last_out.time) : '—';
    // Newest first, as the server sorts them.
    $('acs-list').replaceChildren(...data.events.map(event => {
      const row = el('div', null, 'row'), body = el('div', null, 'row-body');
      body.append(el('strong', event.direction === 'in' ? 'Вход' : 'Выход', event.direction === 'in' ? 'acs-in' : ''));
      if (event.zone) body.append(el('div', event.zone, 'meta'));
      row.append(body, el('span', zoned(event.time), 'mono'));
      return row;
    }));
    if (!data.events.length) { $('acs-note').textContent = 'За последнюю неделю проходов нет.'; $('acs-note').hidden = false; }
  } catch (error) {
    $('acs-list').replaceChildren();
    $('acs-note').textContent = error.message; $('acs-note').hidden = false;
  } finally { $('acs-refresh').disabled = false; }
}

async function start() {
  showError('');
  $('start').disabled = true;
  $('status').hidden = false;
  $('status').textContent = STATUS.waiting;
  try {
    const result = await api('web/logins', { method: 'POST' });
    job = result;
    poll(result.id);
  } catch (error) { stop(error.message); }
}

function stop(message) {
  job = null;
  $('qr').hidden = true; $('qr-link').hidden = true;
  $('status').textContent = message;
  $('start').disabled = false;
  $('start').textContent = 'Начать заново';
}

async function poll(id) {
  if (!job || job.id !== id) return;
  const headers = { 'X-Login-Token': job.token };
  try {
    const state = await api('web/logins/' + encodeURIComponent(id), { headers });
    if (!job || job.id !== id) return;
    $('status').textContent = state.error || STATUS[state.status] || state.status;
    if (state.qr_png) { $('qr').src = state.qr_png; $('qr').hidden = false; $('qr-link').href = state.qr; $('qr-link').hidden = false; }
    else { $('qr').hidden = true; $('qr-link').hidden = true; }
    if (state.status === 'complete') {
      await api('web/logins/' + encodeURIComponent(id) + '/claim', { method: 'POST', headers });
      job = null; needFresh = false;
      $('status').hidden = true; $('start').disabled = false;
      // Straight back to the site after signing in for it; the site itself says
      // when the server has no working MIREA session for the person.
      if (handoff) { await continueToSite(); return; }
      await load();
      return;
    }
    if (['expired', 'failed', 'cancelled'].includes(state.status)) { stop(state.error || STATUS[state.status]); return; }
  } catch (error) {
    // The phone may be busy in the MIREA app; keep polling through network blips.
    if (error instanceof TypeError) { $('status').textContent = 'Нет связи с сервером, пробуем снова…'; setTimeout(() => poll(id), 3000); return; }
    stop(error.message); return;
  }
  setTimeout(() => poll(id), 2000);
}

function confirmChanges(lines) {
  const dialog = $('confirm-dialog');
  $('confirm-list').replaceChildren(...lines.map(text => { const item = document.createElement('li'); item.textContent = text; return item; }));
  if (typeof dialog.showModal !== 'function') return Promise.resolve(confirm('Сохранить изменения?\n\n' + lines.join('\n')));
  dialog.returnValue = '';
  dialog.showModal();
  return new Promise(resolve => dialog.addEventListener('close', () => resolve(dialog.returnValue === 'ok'), { once: true }));
}

$('start').addEventListener('click', start);
$('skip-fresh').addEventListener('click', () => { needFresh = false; $('skip-fresh').hidden = true; continueToSite(); });
$('continue').addEventListener('click', continueToSite);
$('settings').addEventListener('submit', async event => {
  event.preventDefault();
  const body = {};
  if ($('name').value.trim() !== account.label) body.label = $('name').value.trim();
  const username = $('username').value.trim().replace(/^@/, '').toLowerCase();
  const usernameChanged = username && username !== ((lastMe && lastMe.username) || '');
  if (!Object.keys(body).length && !usernameChanged) { $('settings-status').textContent = 'Нет изменений'; return; }
  // Every change is confirmed in a dialog before it is saved.
  const lines = [];
  if (usernameChanged) lines.push('Username: @' + username);
  if ('label' in body) lines.push('Имя на паре: «' + body.label + '»');
  if (!await confirmChanges(lines)) { $('settings-status').textContent = 'Не сохранено'; return; }
  try {
    if (usernameChanged) {
      const result = await api('web/username', { method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ username }) });
      lastMe.username = result.username;
    }
    if ('label' in body) await api('web/account', { method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
    $('settings-status').textContent = 'Сохранено';
    await loadSettings();
  } catch (error) { $('settings-status').textContent = error.message; }
});
$('pass-toggle').addEventListener('click', () => {
  const allow = !pass.pass_allowed;
  const lines = [allow
    ? 'Разрешить пропуск людям из списка (' + pass.people.length + ').'
    : 'Запретить пропуск всем. Даже люди из списка не смогут им пользоваться, пока вы снова не разрешите. Список сохранится.'];
  passChange(lines, async () => {
    await api('web/account', { method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ pass_allowed: allow }) });
    return api('web/pass');
  }, $('pass-toggle'));
});
$('people-form').addEventListener('submit', searchPeople);
$('acs-refresh').addEventListener('click', loadAttendance);
$('logout').addEventListener('click', async () => {
  try { await api('web/logout', { method: 'POST' }); } catch (_) { /* signed out locally anyway */ }
  load();
});
load();
