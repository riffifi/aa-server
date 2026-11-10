'use strict';
const element = id => document.getElementById(id);
let sequence = 0;
let hiddenAt = 0;
let enrollmentBusy = false;
let retryAt = 0;
async function passAPI(path, options = {}) {
  const response = await fetch(path, { credentials: 'same-origin', cache: 'no-store', ...options });
  const result = await response.json();
  if (!response.ok) {
    if (response.status === 401) {
      clearNumbers();
      element('pass-app').hidden = true;
      element('pass-signin').hidden = false;
    }
    throw new Error(typeof result.detail === 'string' ? result.detail : 'Не удалось выполнить запрос. Проверьте код и попробуйте ещё раз.');
  }
  return result;
}
function report(error) {
  element('pass-error').textContent = error.message;
  element('pass-error').hidden = false;
}
function clearNumbers() {
  sequence++;
  hiddenAt = 0;
  document.querySelectorAll('.pass-number').forEach(node => { node.textContent = ''; });
}
async function loadPasses() {
  clearNumbers();
  element('pass-error').hidden = true;
  const requestSequence = sequence;
  try {
    const result = await passAPI('/site/passes');
    if (requestSequence !== sequence) return;
    element('pass-signin').hidden = true;
    element('pass-app').hidden = false;
    element('pass-enroll').hidden = !result.passes.some(pass => pass.own && pass.pass_allowed);
    const nodes = result.passes.map(pass => {
      const container = document.createElement('div');
      container.className = 'block';
      const label = document.createElement('p');
      label.textContent = (pass.label || pass.username || 'Без имени') + (pass.own ? ' · ваш пропуск' : '');
      const number = document.createElement('p');
      number.className = 'pass-number';
      const button = document.createElement('button');
      button.className = 'secondary';
      button.textContent = 'Показать номер';
      if (!pass.pass_allowed) {
        button.disabled = true;
        label.textContent += ' · выключен в настройках AA ID';
      }
      button.addEventListener('click', async () => {
        clearNumbers();
        const requestSequence = sequence;
        button.disabled = true;
        try {
          const data = await passAPI('/site/passes/' + encodeURIComponent(pass.account_id));
          if (requestSequence !== sequence || document.hidden) return;
          number.textContent = data.number || 'Пропуск ещё не подключён';
          hiddenAt = Date.now() + 30000;
        } catch (error) { report(error); }
        finally { button.disabled = false; }
      });
      container.append(label, button, number);
      return container;
    });
    if (!nodes.length) {
      const empty = document.createElement('p');
      empty.textContent = 'Нет доступных пропусков. Подключите аккаунт МИРЭА через AA ID.';
      nodes.push(empty);
    }
    element('pass-list').replaceChildren(...nodes);
  } catch (error) { report(error); }
}
function updateButtons() {
  element('pass-code').disabled = enrollmentBusy || Date.now() < retryAt;
  element('pass-issue').disabled = enrollmentBusy;
}
async function enroll(issue) {
  if (enrollmentBusy) return;
  enrollmentBusy = true;
  updateButtons();
  element('pass-error').hidden = true;
  try {
    const options = { method: 'POST' };
    if (issue) {
      options.headers = { 'Content-Type': 'application/json' };
      options.body = JSON.stringify({ code: element('pass-input').value });
    }
    const result = await passAPI('/site/pass/' + (issue ? 'issue' : 'code'), options);
    if (issue) {
      element('pass-input').value = '';
      element('pass-status').textContent = 'Пропуск сохранён на сервере.';
      await loadPasses();
    } else {
      retryAt = typeof result.retry_at === 'number' ? result.retry_at * 1000 : Date.now() + 60000;
      const messages = { code_sent: 'Код отправлен способом подтверждения МИРЭА.', wait: 'МИРЭА просит подождать перед повторной отправкой.', unavailable: 'У аккаунта нет цифрового пропуска или способа подтверждения в МИРЭА.' };
      element('pass-status').textContent = (messages[result.state] || 'Неизвестный ответ МИРЭА.') + (result.retry_at ? ' Повторный запрос: ' + new Date(retryAt).toLocaleTimeString('ru-RU') + '.' : '');
    }
  } catch (error) { report(error); }
  finally { enrollmentBusy = false; updateButtons(); }
}
element('pass-refresh').addEventListener('click', loadPasses);
element('pass-code').addEventListener('click', () => enroll(false));
element('pass-form').addEventListener('submit', event => { event.preventDefault(); enroll(true); });
document.addEventListener('visibilitychange', () => { if (document.hidden) clearNumbers(); });
window.addEventListener('pagehide', clearNumbers);
setInterval(() => { if (hiddenAt && Date.now() >= hiddenAt) clearNumbers(); updateButtons(); }, 1000);
loadPasses();
