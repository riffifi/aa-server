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

// Without a site session, go to AA ID right away: a person signed in there comes
// straight back without seeing a sign-in page. Not after a failed sign-in or a sign-out.
function autoSignIn() {
  const params = new URLSearchParams(location.search);
  if (params.has('signin') || params.has('signedout')) return false;
  try { if (Date.now() - Number(sessionStorage.getItem('aa-auto-signin') || 0) < 60000) return false; sessionStorage.setItem('aa-auto-signin', String(Date.now())); } catch (_) { /* no storage */ }
  location.assign('/auth/aaid');
  return true;
}

function reconnect() {
  try { if (Date.now() - Number(localStorage.getItem('aa-reconnect') || 0) < 6 * 3600 * 1000) return false; localStorage.setItem('aa-reconnect', String(Date.now())); } catch (_) { return false; }
  location.assign('/auth/aaid?fresh=1');
  return true;
}

async function load() {
  try {
    state = await api('/site/state');
    showError('');
    render();
  } catch (error) {
    if (error.status === 401 && autoSignIn()) return;
    if (error.status === 401) { $('app').hidden = true; $('signin').hidden = false; $('logout').hidden = true; $('aaid-link').hidden = true; $('who').textContent = ''; return; }
    showError(error.message);
  }
}

function render() {
  $('signin').hidden = true; $('app').hidden = false; $('logout').hidden = false;
  const me = state.me;
  $('who').textContent = [me.label && me.label !== 'Без имени' ? me.label : null, me.group_name].filter(Boolean).join(' · ');
  $('connect').hidden = me.connected;
  // Not connected (an AA ID made before the server took MIREA sessions, or an expired
  // session): others cannot see or mark this person, so ask for a MIREA QR sign-in once.
  if (!me.connected && reconnect()) return;
  if (state.aaid_url) { $('aaid-link').href = $('name-link').href = state.aaid_url.replace(/\/$/, '') + '/#settings'; }
  $('aaid-link').hidden = !state.aaid_url;
  $('name-banner').hidden = !(me.account_id && me.label === 'Без имени' && state.aaid_url);
  $('schedule-link').href = state.schedule_url;

  renderLesson();
  $('mark-group').hidden = $('mark-group-note').hidden = !(me.group_id && state.own_group_size > 0);

  $('groups-block').hidden = $('people-block').hidden = !state.active;
  $('people-heading').textContent = state.active === 'soon' ? 'Люди на следующей паре' : 'Люди на паре';
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

function inMinutes(seconds) {
  const minutes = Math.max(1, Math.ceil(seconds / 60));
  return minutes < 60 ? minutes + ' мин' : Math.floor(minutes / 60) + ' ч' + (minutes % 60 ? ' ' + minutes % 60 + ' мин' : '');
}
const describe = lesson => [lesson.type, lesson.location, lesson.teacher].filter(Boolean).join(' · ');

// The current lesson, or during a break the next one with a countdown to it.
function renderLesson() {
  const { lesson, next, me } = state;
  const now = Date.now() / 1000;
  let countdown = '';
  if (lesson) {
    $('lesson-label').textContent = 'Сейчас · ' + lesson.start + '–' + lesson.end;
    $('lesson-title').textContent = lesson.subject;
    $('lesson-meta').textContent = describe(lesson);
    countdown = 'До конца ' + inMinutes(lesson.ends_at - now);
    $('lesson-next').textContent = next ? 'Далее ' + next.start + '–' + next.end + ': ' + next.subject + (next.location ? ' · ' + next.location : '') : 'Это последняя пара сегодня';
  } else if (next) {
    $('lesson-label').textContent = (state.had_lessons ? 'Перерыв · ' : 'Следующая пара · ') + next.start + '–' + next.end;
    $('lesson-title').textContent = next.subject;
    $('lesson-meta').textContent = describe(next);
    countdown = 'Начнётся через ' + inMinutes(next.starts_at - now);
    $('lesson-next').textContent = '';
  } else {
    $('lesson-label').textContent = 'Сейчас';
    $('lesson-title').textContent = state.schedule_error || (me.group_id ? 'Сегодня пар больше нет' : 'Группа не определена');
    $('lesson-meta').textContent = '';
    $('lesson-next').textContent = '';
  }
  $('lesson-countdown').textContent = countdown;
  $('lesson-countdown').hidden = !countdown;
  // A lesson just started or ended: ask the server for the new state.
  const edge = lesson ? lesson.ends_at : next ? next.starts_at - 15 * 60 : null;
  if (edge && now >= edge && now - edge < 60 && $('scanner').hidden) load();
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
  if (person.username) meta.append(el('span', '@' + person.username, ''), document.createTextNode(' · '));
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
// BarcodeDetector where the browser has it, jsQR otherwise (and as a second opinion),
// on the whole frame and on the centre square, for light and dark codes.
let stream = null, scanning = false, sending = false, lastToken = null, lastAt = 0, detector = null, frameNumber = 0;
let mode = 'selected', pending = null, cropTurn = 0;
const gaveUp = new Set(); // sessions that ended: retrying them cannot help
const canvas = document.createElement('canvas');
const context2d = canvas.getContext('2d', { willReadFrequently: true });

function lessonToken(text) {
  try {
    const url = new URL(text.trim());
    if (url.protocol !== 'https:' || !['pulse.mirea.ru', 'attendance-app.mirea.ru'].includes(url.hostname)) return null;
    return url.searchParams.get('token');
  } catch (_) { return null; }
}

async function makeDetector() {
  try {
    if ('BarcodeDetector' in window && (await BarcodeDetector.getSupportedFormats()).includes('qr_code')) return new BarcodeDetector({ formats: ['qr_code'] });
  } catch (_) { /* not supported */ }
  return null;
}

function cameraError(error) {
  if (!window.isSecureContext) return 'Камера работает только по https. Вставьте ссылку из QR ниже или сфотографируйте его.';
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) return 'Этот браузер не даёт доступ к камере. Сфотографируйте QR или вставьте ссылку.';
  if (error && error.name === 'NotAllowedError') return 'Доступ к камере запрещён. Разрешите камеру для сайта в настройках браузера или сфотографируйте QR.';
  if (error && (error.name === 'NotFoundError' || error.name === 'OverconstrainedError')) return 'Камера не найдена. Сфотографируйте QR или вставьте ссылку.';
  if (error && error.name === 'NotReadableError') return 'Камера занята другим приложением. Закройте его и откройте сканер снова.';
  return 'Камера недоступна. Сфотографируйте QR или вставьте ссылку из него.';
}

async function startCamera(deviceId) {
  stopCamera();
  const video = { width: { ideal: 1920 }, height: { ideal: 1080 } };
  if (deviceId) video.deviceId = { exact: deviceId }; else video.facingMode = { ideal: 'environment' };
  try {
    stream = await navigator.mediaDevices.getUserMedia({ video, audio: false });
  } catch (error) {
    if (error && error.name === 'OverconstrainedError') stream = await navigator.mediaDevices.getUserMedia({ video: true, audio: false });
    else throw error;
  }
  const track = stream.getVideoTracks()[0];
  try { await track.applyConstraints({ advanced: [{ focusMode: 'continuous' }] }); } catch (_) { /* not supported */ }
  const capabilities = track.getCapabilities ? track.getCapabilities() : {};
  $('torch').hidden = !capabilities.torch; $('torch').setAttribute('aria-pressed', 'false');
  $('video').srcObject = stream;
  await $('video').play();
  await listCameras(track.getSettings ? track.getSettings().deviceId : null);
  scanning = true;
  scanLoop();
}

async function listCameras(current) {
  try {
    const cameras = (await navigator.mediaDevices.enumerateDevices()).filter(device => device.kind === 'videoinput');
    $('camera-select').hidden = cameras.length < 2;
    $('camera-select').replaceChildren(...cameras.map((camera, index) => {
      const option = el('option', camera.label || 'Камера ' + (index + 1)); option.value = camera.deviceId; option.selected = camera.deviceId === current;
      return option;
    }));
  } catch (_) { $('camera-select').hidden = true; }
}

function stopCamera() {
  scanning = false;
  if (stream) stream.getTracks().forEach(track => track.stop());
  stream = null; $('video').srcObject = null;
}

async function openScanner(target) {
  mode = target === 'group' ? 'group' : 'selected';
  pending = null; gaveUp.clear(); shownResults.clear(); lastToken = null;
  $('scanner').hidden = false; $('scanner-results').replaceChildren(); $('paste').value = ''; $('photo').value = '';
  $('scanner-target').textContent = mode === 'group'
    ? 'Отмечаем всех подключённых из группы ' + (state.me.group_name || '') + ', даже если пары нет в расписании.'
    : 'Отмечаем выбранных: ' + selectedPending().length + '.';
  $('scanner-status').textContent = 'Наведите камеру на QR-код занятия.';
  detector = await makeDetector();
  try {
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) throw Error('no camera api');
    await startCamera(null);
  } catch (error) {
    $('scanner-status').textContent = cameraError(error);
  }
}

function closeScanner() {
  stopCamera();
  $('scanner').hidden = true;
  load();
}

function decodeImage(source, width, height) {
  // jsQR on the centre square (where people aim) and on the whole frame, alternating.
  const results = [];
  const crops = cropTurn++ % 2 ? [[0, 0, width, height, 1000]] : [[(width - Math.min(width, height) * .75) / 2, (height - Math.min(width, height) * .75) / 2, Math.min(width, height) * .75, Math.min(width, height) * .75, 800]];
  for (const [x, y, w, h, limit] of crops) {
    const scale = Math.min(1, limit / Math.max(w, h));
    canvas.width = Math.round(w * scale); canvas.height = Math.round(h * scale);
    context2d.drawImage(source, x, y, w, h, 0, 0, canvas.width, canvas.height);
    const image = context2d.getImageData(0, 0, canvas.width, canvas.height);
    const code = window.jsQR && jsQR(image.data, image.width, image.height, { inversionAttempts: 'attemptBoth' });
    if (code && code.data) results.push(code.data);
  }
  return results[0] || null;
}

async function readCode(source, width, height) {
  if (detector) {
    try {
      const codes = await detector.detect(source);
      const found = codes.find(code => lessonToken(code.rawValue)) || codes[0];
      if (found) return found.rawValue;
    } catch (_) { /* fall back to jsQR */ }
    // The native detector misses some dense or dark codes; ask jsQR every other frame.
    if (frameNumber++ % 2) return null;
  }
  return decodeImage(source, width, height);
}

async function scanLoop() {
  const video = $('video');
  while (scanning) {
    const started = performance.now();
    if (!sending && video.videoWidth) {
      let text = null;
      try { text = await readCode(video, video.videoWidth, video.videoHeight); } catch (_) { text = null; }
      if (text && scanning) { found(); submit(text); }
    }
    await new Promise(resolve => setTimeout(resolve, Math.max(60, 180 - (performance.now() - started))));
  }
}

function found() {
  $('viewfinder').classList.add('found');
  setTimeout(() => $('viewfinder').classList.remove('found'), 300);
}

async function readPhoto(file) {
  if (!file) return;
  $('scanner-status').textContent = 'Ищем QR на фото…';
  try {
    const bitmap = await createImageBitmap(file);
    detector = detector || await makeDetector();
    let text = null;
    if (detector) { try { const codes = await detector.detect(bitmap); text = codes.length ? codes[0].rawValue : null; } catch (_) { text = null; } }
    // Several scales: a QR far away needs detail, a close one is decoded faster small.
    for (const limit of [1600, 1000, 2400]) {
      if (text) break;
      const scale = Math.min(1, limit / Math.max(bitmap.width, bitmap.height));
      canvas.width = Math.round(bitmap.width * scale); canvas.height = Math.round(bitmap.height * scale);
      context2d.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
      const image = context2d.getImageData(0, 0, canvas.width, canvas.height);
      const code = window.jsQR && jsQR(image.data, image.width, image.height, { inversionAttempts: 'attemptBoth' });
      text = code ? code.data : null;
    }
    if (!text) { $('scanner-status').textContent = 'На фото не нашли QR. Снимите ближе и ровнее.'; return; }
    lastToken = null;
    await submit(text);
  } catch (_) {
    $('scanner-status').textContent = 'Не удалось открыть фото.';
  } finally { $('photo').value = ''; }
}

function selectedPending() {
  return [...selected].filter(id => { const p = state.people.find(x => x.id === id); return p && markable(p) && !gaveUp.has(id); });
}

async function submit(text) {
  const token = lessonToken(text);
  if (!token) { $('scanner-status').textContent = 'Это не QR-код посещаемости Пульса.'; return; }
  // A stationary QR is seen on many frames; retry the same token only after a pause.
  if (token === lastToken && Date.now() - lastAt < 2500) return;
  const ids = mode === 'group' ? pending : selectedPending();
  if (ids !== null && !ids.length) { $('scanner-status').textContent = 'Все уже отмечены.'; return; }
  lastToken = token; lastAt = Date.now(); sending = true;
  $('scanner-status').textContent = 'QR найден, отмечаем' + (ids ? ' ' + ids.length : '') + '…';
  try { navigator.vibrate && navigator.vibrate(40); } catch (_) { /* no vibration */ }
  try {
    const body = mode === 'group' ? { qr: text, own_group: true, ...(ids ? { account_ids: ids } : {}) } : { qr: text, account_ids: ids };
    const result = await api('/site/mark', json(body));
    for (const item of result.results) {
      if (item.state === 'expired') gaveUp.add(item.id);
      const person = state.people.find(p => p.id === item.id);
      if (person) person.mark = { state: item.state, message: item.message, created: Date.now() / 1000 };
    }
    showResults(result.results);
    const left = mode === 'group'
      ? result.results.filter(item => item.state !== 'approved' && !gaveUp.has(item.id)).map(item => item.id)
      : selectedPending();
    if (mode === 'group') pending = left;
    $('scanner-status').textContent = left.length ? 'Осталось ' + left.length + '. Держите камеру на QR, код обновляется.' : 'Готово, все отмечены.';
    if (!left.length) setTimeout(closeScanner, 1500);
  } catch (error) {
    $('scanner-status').textContent = error.message;
  } finally { sending = false; }
}

const shownResults = new Map();
function showResults(results) {
  for (const item of results) shownResults.set(item.id, item);
  $('scanner-results').replaceChildren(...[...shownResults.values()].map(item => {
    const line = el('div', null, 'row');
    const [, tone] = MARK[item.state] || ['', ''];
    line.append(el('strong', item.label || item.id), el('span', item.message, 'mark ' + tone));
    return line;
  }));
}

$('mark').addEventListener('click', () => openScanner('selected'));
$('mark-group').addEventListener('click', () => openScanner('group'));
$('photo').addEventListener('change', () => readPhoto($('photo').files[0]));
$('camera-select').addEventListener('change', () => startCamera($('camera-select').value).catch(error => { $('scanner-status').textContent = cameraError(error); }));
$('torch').addEventListener('click', async () => {
  const track = stream && stream.getVideoTracks()[0];
  const on = $('torch').getAttribute('aria-pressed') !== 'true';
  try { await track.applyConstraints({ advanced: [{ torch: on }] }); $('torch').setAttribute('aria-pressed', String(on)); $('torch').textContent = on ? 'Выключить фонарик' : 'Фонарик'; } catch (_) { $('torch').hidden = true; }
});
$('scanner-close').addEventListener('click', closeScanner);
$('paste-form').addEventListener('submit', event => { event.preventDefault(); lastToken = null; submit($('paste').value); });
$('select-unmarked').addEventListener('click', () => { touched = true; selected.clear(); state.people.filter(markable).forEach(p => selected.add(p.id)); render(); });
$('logout').addEventListener('click', async () => {
  try { await api('/site/logout', { method: 'POST' }); } catch (_) { /* signed out anyway */ }
  history.replaceState(null, '', '/?signedout=1'); load();
});
document.addEventListener('keydown', event => { if (event.key === 'Escape' && !$('scanner').hidden) closeScanner(); });
if (new URLSearchParams(location.search).get('signin') === 'failed') showError('Не удалось войти через AA ID. Попробуйте ещё раз.');
load();
setInterval(() => { if (!document.hidden && $('scanner').hidden) load(); }, 15000);
setInterval(() => { if (state && !document.hidden) renderLesson(); }, 20000);
document.addEventListener('visibilitychange', () => { if (!document.hidden && $('scanner').hidden) load(); });
