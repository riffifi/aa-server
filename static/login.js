'use strict';
const $ = id => document.getElementById(id);
let currentJob = null, currentBase = null;

const STATUS = {
  waiting: 'Ожидаем свободный слот…',
  login: 'Отсканируйте QR в приложении MIREA или откройте ссылку ниже.',
  finishing: 'Подтверждение получено. Завершаем сессию…',
  complete: 'Готово! Открываем вашу страницу…',
  expired: 'Вход истёк. Начните заново.',
  failed: 'Не удалось. Начните заново.',
  cancelled: 'Отменено.'
};

async function start() {
  $('retry').hidden = true;
  $('qr').hidden = true; $('qr-link').hidden = true;
  $('status').textContent = 'Готовим безопасный вход…';
  try {
    const response = await fetch('/api/logins', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Не удалось начать вход.');
    currentBase = result.enrollment_path; currentJob = result.id;
    poll();
  } catch (error) { fail(error.message); }
}

function fail(message) {
  currentJob = null;
  $('status').textContent = message;
  $('qr').hidden = true; $('qr-link').hidden = true;
  $('retry').hidden = false;
}

async function poll() {
  const job = currentJob, base = currentBase;
  if (!job) return;
  try {
    const response = await fetch(base + '/state', { cache: 'no-store' });
    if (!response.ok) throw Error('Вход истёк. Начните заново.');
    const result = await response.json();
    if (job !== currentJob) return;
    $('status').textContent = result.error || STATUS[result.status] || result.status;
    if (result.qr) { $('qr').src = base + '/qr?t=' + Date.now(); $('qr').hidden = false; $('qr-link').href = result.qr; $('qr-link').hidden = false; }
    else { $('qr').hidden = true; $('qr-link').hidden = true; }
    if (result.status === 'complete') {
      currentJob = null;
      try {
        const claimed = await fetch(base + '/claim', { method: 'POST', cache: 'no-store' });
        const data = await claimed.json();
        if (claimed.ok && data.account_id) { location.href = '/u/' + encodeURIComponent(data.account_id); return; }
        throw Error('Не удалось открыть страницу аккаунта.');
      } catch (error) { fail(error.message); return; }
    }
    if (['expired', 'failed', 'cancelled'].includes(result.status)) { fail(STATUS[result.status]); return; }
  } catch (error) { fail(error.message); return; }
  if (currentJob === job) setTimeout(poll, 2000);
}

$('retry').onclick = () => start();
start();
