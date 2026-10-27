'use strict';
const $ = id => document.getElementById(id);
const MARK = {
  approved: ['✓', 'ok', 'Отмечен'], waiting: ['…', 'wait', 'Ждёт подтверждения'], rejected: ['✗', 'bad', 'Не отмечен'],
  expired: ['✗', 'bad', 'Сессия истекла'], error: ['✗', 'bad', 'Ошибка']
};
const MIREA = {
  present: ['✓', 'ok', 'Отмечен в журнале МИРЭА'], excused: ['У', 'wait', 'Уважительная причина в журнале МИРЭА'],
  absent: ['✗', 'bad', 'Отсутствовал по журналу МИРЭА']
};
function el(tag, text, className) { const n = document.createElement(tag); if (text) n.textContent = text; if (className) n.className = className; return n; }
function showError(message) { $('error').textContent = message || ''; $('error').hidden = !message; }
const time = s => new Date(s * 1000).toLocaleTimeString('ru-RU', { timeZone: 'Europe/Moscow', hour: '2-digit', minute: '2-digit' });

function render(data) {
  $('today-date').textContent = 'Сегодня, ' + data.date;
  $('who').textContent = data.group_name || '';
  const table = $('today-table');
  const empty = !data.group_id ? 'Группа не известна: войдите через AA ID заново или укажите группу в настройках.'
    : data.schedule_error || (!data.lessons.length ? 'Сегодня пар нет.' : '');
  $('today-empty').textContent = empty; $('today-empty').hidden = !empty; table.hidden = Boolean(empty);
  if (empty) return;
  const head = el('tr');
  head.append(el('th', 'Человек'));
  for (const lesson of data.lessons) {
    const th = el('th', null, 'col ' + lesson.state);
    th.append(el('span', lesson.start + '–' + lesson.end, 'caption'), el('strong', lesson.subject));
    const marked = Object.keys(lesson.marks).filter(id => lesson.mirea[id] === 'present' || (lesson.marks[id] && lesson.marks[id].state === 'approved')).length;
    th.append(el('span', [lesson.type, lesson.location].filter(Boolean).join(' · '), 'caption'),
      el('span', `${marked} из ${Object.keys(lesson.marks).length}` + (lesson.groups.length > 1 ? ' · ' + lesson.groups.map(g => g.name).join(', ') : ''), 'caption'));
    head.append(th);
  }
  const rows = [];
  let group = null;
  for (const person of data.people) {
    if (person.group_name !== group) {
      group = person.group_name;
      const tr = el('tr', null, 'group-row'), td = el('td', group || 'Без группы'); td.colSpan = data.lessons.length + 1; tr.append(td); rows.push(tr);
    }
    const tr = el('tr', null, person.own ? 'own' : '');
    const name = el('td', null, 'person');
    name.append(el('span', person.label));
    if (person.username) name.append(el('span', ' @' + person.username, 'caption'));
    if (person.session !== 'valid') name.append(el('span', ' · не подключён', 'caption'));
    tr.append(name);
    for (const lesson of data.lessons) {
      if (!(person.id in lesson.marks)) { tr.append(el('td', '', 'off')); continue; }
      const mark = lesson.marks[person.id], mirea = lesson.mirea[person.id];
      // The Pulse journal wins when it is definite: present or excused at any time,
      // absent once the lesson is over (during it everyone is absent until marked).
      if (mirea === 'present' || mirea === 'excused' || (mirea === 'absent' && lesson.state === 'past' && !(mark && mark.state === 'approved'))) {
        const [symbol, tone, title] = MIREA[mirea];
        const td = el('td', symbol, 'mark ' + tone); td.title = title; tr.append(td); continue;
      }
      if (!mark) { tr.append(el('td', '—', 'null')); continue; }
      const [symbol, tone, title] = MARK[mark.state] || ['?', '', mark.state];
      const td = el('td', symbol, 'mark ' + tone);
      td.title = `${title}, ${time(mark.created)}${mark.message ? ': ' + mark.message : ''}`;
      tr.append(td);
    }
    rows.push(tr);
  }
  table.replaceChildren(el('thead'), el('tbody'));
  table.tHead.append(head); table.tBodies[0].append(...rows);
}

async function load() {
  try {
    const response = await fetch('/site/today', { cache: 'no-store', credentials: 'same-origin' });
    if (response.status === 401) { location.assign('/auth/aaid'); return; }
    const data = await response.json();
    if (!response.ok) throw Error(typeof data.detail === 'string' ? data.detail : 'Сервер недоступен.');
    render(data); showError('');
    const errors = Object.values(data.mirea || {}).filter(j => j.error).map(j => j.error);
    $('today-updated').textContent = 'Обновлено в ' + new Date().toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' }) + ', обновляется само каждую минуту. '
      + (Object.keys(data.mirea || {}).length ? 'Статусы из журнала МИРЭА подтягиваются раз в 2 минуты.' : '') + (errors.length ? ' МИРЭА: ' + [...new Set(errors)].join('; ') + '.' : '');
  } catch (error) { showError(error.message || 'Сервер недоступен.'); }
}
load();
setInterval(() => { if (!document.hidden) load(); }, 60000);
document.addEventListener('visibilitychange', () => { if (!document.hidden) load(); });
