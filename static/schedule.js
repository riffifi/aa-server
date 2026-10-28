'use strict';
const $ = id => document.getElementById(id);
const DAYS = ['Понедельник', 'Вторник', 'Среда', 'Четверг', 'Пятница', 'Суббота', 'Воскресенье'];
const params = new URLSearchParams(location.search);
let group = Number(params.get('g')) || 0, name = params.get('n') || '', week = Number(params.get('w')) || 0, data = null;

function el(tag, text, className) { const n = document.createElement(tag); if (text) n.textContent = text; if (className) n.className = className; return n; }
function showError(message) { $('error').textContent = message || ''; $('error').hidden = !message; }
const dayTitle = iso => { const d = new Date(iso + 'T12:00:00'); return d.toLocaleDateString('ru-RU', { day: 'numeric', month: 'long' }); };

async function get(path) {
  const response = await fetch(path, { cache: 'no-store', credentials: 'same-origin' });
  if (response.status === 401) { location.assign('/auth/aaid'); throw Error('Нужен вход'); }
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw Error(typeof body.detail === 'string' ? body.detail : 'Сервер недоступен.');
  return body;
}

function remember() {
  const query = new URLSearchParams();
  if (group && data && !data.own) { query.set('g', group); if (name) query.set('n', name); }
  if (week) query.set('w', week);
  history.replaceState(null, '', '/schedule' + (query.toString() ? '?' + query : ''));
}

function lessonRow(lesson) {
  const row = el('div', null, 'slot ' + lesson.state);
  const time = el('div', null, 'slot-time mono');
  time.append(el('span', lesson.start), el('span', lesson.end, 'caption'));
  const body = el('div', null, 'slot-body');
  const title = el('strong', lesson.subject);
  if (lesson.state === 'now') title.append(el('span', 'сейчас', 'badge'));
  body.append(title, el('span', [lesson.type, lesson.location, lesson.teacher].filter(Boolean).join(' · '), 'meta'));
  row.append(time, body);
  return row;
}

function render() {
  const monday = new Date(data.monday + 'T12:00:00'), sunday = new Date(monday.getTime() + 6 * 86400000);
  const range = monday.toLocaleDateString('ru-RU', { day: 'numeric', month: 'long' }) + ' — ' + sunday.toLocaleDateString('ru-RU', { day: 'numeric', month: 'long' });
  $('week-label').textContent = (week === 0 ? 'Эта неделя' : week === 1 ? 'Следующая неделя' : week === -1 ? 'Прошлая неделя' : 'Неделя') + ' · ' + range;
  $('group-title').textContent = data.group_name || (data.group_id ? 'Группа ' + data.group_id : 'Группа не выбрана');
  $('current').disabled = week === 0;
  $('mine').hidden = data.own || !data.own_group_id;
  if (data.own_group_name) $('mine').textContent = 'Моя группа: ' + data.own_group_name;
  const days = data.days.filter(day => day.lessons.length || (day.today && day.weekday < 6));
  $('week').replaceChildren(...days.map(day => {
    const block = el('section', null, 'day' + (day.today ? ' today' : ''));
    const head = el('div', null, 'block-head');
    head.append(el('h2', DAYS[day.weekday] + ', ' + dayTitle(day.date), 'heading'), el('span', day.today ? 'сегодня' : day.lessons.length + ' ' + plural(day.lessons.length), 'caption'));
    block.append(head);
    const list = el('div', null, 'list');
    if (day.lessons.length) list.append(...day.lessons.map(lessonRow)); else list.append(el('p', 'Пар нет.', 'note'));
    block.append(list);
    return block;
  }));
  const empty = !data.group_id ? 'Группа не известна. Найдите свою группу через поиск выше.' : data.schedule_error || (!data.days.some(d => d.lessons.length) ? 'На этой неделе пар нет.' : '');
  $('empty').textContent = empty; $('empty').hidden = !empty;
  if (!data.schedule_error) { const today = document.querySelector('.day.today'); if (today && week === 0 && !render.scrolled) { render.scrolled = true; today.scrollIntoView({ block: 'start' }); } }
}
function plural(n) { return n % 10 === 1 && n % 100 !== 11 ? 'пара' : [2, 3, 4].includes(n % 10) && ![12, 13, 14].includes(n % 100) ? 'пары' : 'пар'; }

async function load() {
  try {
    const query = new URLSearchParams({ week });
    if (group) { query.set('group', group); if (name) query.set('name', name); }
    data = await get('/site/schedule?' + query);
    render(); remember(); showError('');
  } catch (error) { showError(error.message); }
}

let typing = null;
$('search').addEventListener('input', () => {
  clearTimeout(typing);
  const q = $('search').value.trim();
  if (q.length < 2) { $('results').replaceChildren(); return; }
  typing = setTimeout(async () => {
    try {
      const found = await get('/site/schedule/search?q=' + encodeURIComponent(q));
      $('results').replaceChildren(...found.groups.map(g => {
        const button = el('button', g.name || g.full); button.type = 'button';
        button.onclick = () => { group = g.id; name = g.name; $('search').value = ''; $('results').replaceChildren(); load(); };
        return button;
      }));
      if (!found.groups.length) $('results').replaceChildren(el('span', 'Ничего не найдено', 'caption'));
    } catch (error) { showError(error.message); }
  }, 300);
});
$('search-form').addEventListener('submit', event => { event.preventDefault(); const first = $('results').querySelector('button'); if (first) first.click(); });
$('prev').onclick = () => { week -= 1; load(); };
$('next').onclick = () => { week += 1; load(); };
$('current').onclick = () => { week = 0; load(); };
$('mine').onclick = () => { group = 0; name = ''; load(); };
load();
setInterval(() => { if (!document.hidden && week === 0) load(); }, 60000);
