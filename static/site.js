'use strict';
const $ = id => document.getElementById(id);
let state = null;
const selected = new Set();
let touched = false; // the person changed the selection by hand

const MARK = {
  approved: ['Отмечен', 'ok'], waiting: ['Ждёт подтверждения', 'wait'], rejected: ['Не отмечен', 'bad'],
  expired: ['Сессия истекла', 'bad'], error: ['Ошибка', 'bad']
};

function el(tag, text, className) { const n = document.createElement(tag); if (text) n.textContent = text; if (className) n.className = className; return n; }
function showError(message) { $('error').textContent = message || ''; $('error').hidden = !message; }
async function api(path, options = {}) {
  const response = await fetch(path, { cache: 'no-store', credentials: 'same-origin', ...options });
  let data = {};
  try { data = await response.json(); } catch (_) { /* empty body */ }
  if (!response.ok) { const error = Error(typeof data.detail === 'string' ? data.detail : 'Сервер недоступен.'); error.status = response.status; throw error; }
  return data;
}
const json = body => ({ method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });

async function load() {
  try {
    state = await api('/site/state');
    showError('');
    render();
  } catch (error) {
    if (error.status === 401) { $('app').hidden = true; $('signin').hidden = false; $('logout').hidden = true; $('settings-open').hidden = true; $('who').textContent = ''; return; }
    showError(error.message);
  }
}

function render() {
  $('signin').hidden = true; $('app').hidden = false; $('logout').hidden = false;
  const me = state.me;
  $('who').textContent = [me.label && me.label !== 'Без имени' ? me.label : null, me.group_name].filter(Boolean).join(' · ');
  $('connect').hidden = me.connected;
  $('settings-open').hidden = !me.account_id;
  // Someone new is asked for their name right away.
  if (me.account_id && me.label === 'Без имени' && !settingsSeen) openSettings();
  $('schedule-link').href = state.schedule_url;

  const lesson = state.lesson;
  $('lesson-label').textContent = lesson ? 'Сейчас · ' + lesson.start + '–' + lesson.end : 'Сейчас';
  $('lesson-title').textContent = lesson ? lesson.subject : (state.schedule_error || (me.group_id ? 'Пары нет' : 'Группа не определена'));
  $('lesson-meta').textContent = lesson ? [lesson.type, lesson.location, lesson.teacher].filter(Boolean).join(' · ') : '';
  $('lesson-next').textContent = state.next ? 'Далее ' + state.next.start + '–' + state.next.end + ': ' + state.next.subject + (state.next.location ? ' · ' + state.next.location : '') : '';

  $('groups-block').hidden = $('people-block').hidden = !lesson;
  $('groups').replaceChildren(...state.groups.map(group => {
    const chip = el('span', group.name || 'Группа ' + group.id, 'chip' + (group.own ? ' own' : ''));
    if (group.own) chip.append(el('span', 'ваша', 'badge'));
    return chip;
  }));

  const ids = new Set(state.people.map(p => p.id));
  for (const id of [...selected]) if (!ids.has(id)) selected.delete(id);
  if (!touched) { selected.clear(); state.people.filter(markable).forEach(p => selected.add(p.id)); }
  $('people').replaceChildren(...state.people.map(row));
  $('people-note').hidden = state.people.length > 0;
  $('people-note').textContent = 'На этой паре пока нет людей, подключённых к серверу.';
  updateMarkButton();
}

function markable(person) { return person.session === 'valid' && (!person.mark || person.mark.state !== 'approved'); }

function row(person) {
  const line = el('label', null, 'row person');
  const box = el('input'); box.type = 'checkbox'; box.checked = selected.has(person.id); box.disabled = person.session !== 'valid';
  box.addEventListener('change', () => { touched = true; box.checked ? selected.add(person.id) : selected.delete(person.id); updateMarkButton(); });
  const body = el('span', null, 'row-body');
  const name = el('strong', person.label);
  if (person.own) name.append(el('span', 'вы', 'badge'));
  body.append(name);
  const place = person.inside === true ? 'в вузе' : person.inside === false ? 'не в вузе' : 'нет данных о проходе';
  const meta = el('span', null, 'meta');
  meta.append(el('span', person.group_name || '', ''), document.createTextNode(' · '), el('span', place, person.inside ? 'here' : ''));
  if (person.session !== 'valid') meta.append(document.createTextNode(' · сессия МИРЭА истекла'));
  body.append(meta);
  const [text, tone] = person.mark ? (MARK[person.mark.state] || [person.mark.state, '']) : ['Не отмечался', 'none'];
  const status = el('span', text, 'mark ' + tone);
  if (person.mark && person.mark.message) status.title = person.mark.message;
  line.append(box, body, status);
  return line;
}

function updateMarkButton() {
  $('mark').disabled = selected.size === 0;
  $('mark').textContent = selected.size ? 'Отметить выбранных: ' + selected.size : 'Отметить';
}

// Scanner: the teacher's QR rotates, so keep reading frames and send each new token.
let stream = null, frameTimer = null, sending = false, lastToken = null, lastAt = 0, detector = null;
const canvas = document.createElement('canvas');

function lessonToken(text) {
  try {
    const url = new URL(text.trim());
    if (url.protocol !== 'https:' || !['pulse.mirea.ru', 'attendance-app.mirea.ru'].includes(url.hostname)) return null;
    return url.searchParams.get('token');
  } catch (_) { return null; }
}

async function openScanner() {
  $('scanner').hidden = false; $('scanner-results').replaceChildren(); $('paste').value = '';
  $('scanner-status').textContent = 'Наведите камеру на QR-код занятия.';
  lastToken = null;
  try {
    if ('BarcodeDetector' in window && (await BarcodeDetector.getSupportedFormats()).includes('qr_code')) detector = new BarcodeDetector({ formats: ['qr_code'] });
  } catch (_) { detector = null; }
  try {
    stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: 'environment' }, audio: false });
    $('video').srcObject = stream; await $('video').play();
    frameTimer = setInterval(readFrame, 250);
  } catch (_) {
    $('scanner-status').textContent = 'Камера недоступна. Вставьте ссылку из QR-кода ниже.';
  }
}

function closeScanner() {
  clearInterval(frameTimer); frameTimer = null;
  if (stream) stream.getTracks().forEach(track => track.stop());
  stream = null; $('video').srcObject = null; $('scanner').hidden = true;
  load();
}

async function readFrame() {
  const video = $('video');
  if (sending || !video.videoWidth) return;
  let text = null;
  try {
    if (detector) {
      const codes = await detector.detect(video);
      text = codes.length ? codes[0].rawValue : null;
    } else if (window.jsQR) {
      const scale = Math.min(1, 640 / video.videoWidth);
      canvas.width = Math.round(video.videoWidth * scale); canvas.height = Math.round(video.videoHeight * scale);
      const context = canvas.getContext('2d', { willReadFrequently: true });
      context.drawImage(video, 0, 0, canvas.width, canvas.height);
      const image = context.getImageData(0, 0, canvas.width, canvas.height);
      const code = jsQR(image.data, image.width, image.height, { inversionAttempts: 'dontInvert' });
      text = code ? code.data : null;
    }
  } catch (_) { return; }
  if (text) submit(text);
}

async function submit(text) {
  const token = lessonToken(text);
  if (!token) { $('scanner-status').textContent = 'Это не QR-код посещаемости Пульса.'; return; }
  // A stationary QR is seen on many frames; retry the same token only after a pause.
  if (token === lastToken && Date.now() - lastAt < 2500) return;
  const pending = [...selected].filter(id => { const p = state.people.find(x => x.id === id); return p && markable(p); });
  if (!pending.length) { $('scanner-status').textContent = 'Все выбранные уже отмечены.'; return; }
  lastToken = token; lastAt = Date.now(); sending = true;
  $('scanner-status').textContent = 'Отмечаем ' + pending.length + '…';
  try {
    const result = await api('/site/mark', json({ qr: text, account_ids: pending }));
    for (const item of result.results) {
      const person = state.people.find(p => p.id === item.id);
      if (person) person.mark = { state: item.state, message: item.message, created: Date.now() / 1000 };
    }
    showResults(result.results);
    const left = [...selected].filter(id => { const p = state.people.find(x => x.id === id); return p && markable(p); });
    $('scanner-status').textContent = left.length ? 'Осталось ' + left.length + '. Держите камеру на QR, код обновляется.' : 'Готово, все выбранные отмечены.';
    if (!left.length) setTimeout(closeScanner, 1500);
  } catch (error) {
    $('scanner-status').textContent = error.message;
  } finally { sending = false; }
}

function showResults(results) {
  $('scanner-results').replaceChildren(...results.map(item => {
    const person = state.people.find(p => p.id === item.id);
    const line = el('div', null, 'row');
    const [, tone] = MARK[item.state] || ['', ''];
    line.append(el('strong', person ? person.label : item.id), el('span', item.message, 'mark ' + tone));
    return line;
  }));
}

$('mark').addEventListener('click', openScanner);
$('scanner-close').addEventListener('click', closeScanner);
$('paste-form').addEventListener('submit', event => { event.preventDefault(); lastToken = null; submit($('paste').value); });
$('select-unmarked').addEventListener('click', () => { touched = true; selected.clear(); state.people.filter(markable).forEach(p => selected.add(p.id)); render(); });
let settingsSeen = false;
function openSettings() {
  settingsSeen = true;
  $('name').value = state.me.label === 'Без имени' ? '' : state.me.label;
  $('pass-allowed').checked = Boolean(state.me.pass_allowed);
  $('settings').hidden = false;
  $('settings').scrollIntoView({ behavior: 'smooth', block: 'start' });
}
$('settings-open').addEventListener('click', openSettings);
$('settings-close').addEventListener('click', () => { $('settings').hidden = true; });
$('settings').addEventListener('submit', async event => {
  event.preventDefault();
  const put = (path, body) => api(path, { method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
  try {
    if ($('name').value.trim() !== state.me.label) await put('/site/me/name', { label: $('name').value.trim() });
    if ($('pass-allowed').checked !== Boolean(state.me.pass_allowed)) await put('/site/me/pass', { allowed: $('pass-allowed').checked });
    $('settings').hidden = true; load();
  } catch (error) { showError(error.message); }
});
$('logout').addEventListener('click', async () => { try { await api('/site/logout', { method: 'POST' }); } catch (_) { /* signed out anyway */ } load(); });
document.addEventListener('keydown', event => { if (event.key === 'Escape' && !$('scanner').hidden) closeScanner(); });
if (new URLSearchParams(location.search).get('signin') === 'failed') { showError('Не удалось войти через AA ID. Попробуйте ещё раз.'); history.replaceState(null, '', '/'); }
load();
setInterval(() => { if (!document.hidden && $('scanner').hidden) load(); }, 15000);
document.addEventListener('visibilitychange', () => { if (!document.hidden && $('scanner').hidden) load(); });
