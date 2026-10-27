'use strict';
const $ = id => document.getElementById(id);
let job = null;

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
    render(await api('web/me'));
  } catch (error) {
    $('account').hidden = true; $('signin').hidden = false;
    if (error.status !== 401) showError(error.message);
  }
}

function render(me) {
  const first = $('account').hidden;
  $('signin').hidden = true; $('account').hidden = false;
  if (first) { loadSchedule(); loadAttendance(); }
  $('group').textContent = me.group_name || (me.group_id ? 'Группа ' + me.group_id : 'не определена');
  $('created').textContent = when(me.created);
  $('user-id').textContent = me.id;
  $('device-note').textContent = `В приложении можно войти не больше чем на ${me.max_sessions} устройствах; вход на новом выводит самое давнее. На сайте одновременно открыт один браузер.`;
  $('devices').replaceChildren(...me.sessions.map(device => {
    const row = el('div', null, 'row'), body = el('div', null, 'row-body');
    body.append(el('strong', device.device + (device.current ? ' · это окно' : '')));
    body.append(el('div', (device.kind === 'web' ? 'Сайт' : 'Приложение') + ' · активность ' + when(device.last_used), 'meta'));
    row.append(body);
    if (!device.current) {
      const button = el('button', 'Отключить', 'quiet danger');
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
      body.append(el('div', zoned(event.time) + (event.zone ? ' · ' + event.zone : ''), 'meta'));
      row.append(body);
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
  $('login-progress').hidden = false;
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
  $('start').firstChild.textContent = 'Начать заново ';
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
      job = null;
      $('login-progress').hidden = true; $('start').disabled = false;
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

$('start').addEventListener('click', start);
$('acs-refresh').addEventListener('click', loadAttendance);
$('logout').addEventListener('click', async () => {
  try { await api('web/logout', { method: 'POST' }); } catch (_) { /* signed out locally anyway */ }
  load();
});
load();
