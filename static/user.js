'use strict';
const $ = id => document.getElementById(id);
const accountId = decodeURIComponent(location.pathname.replace(/^\/u\//, '').replace(/\/$/, ''));
let account = null, hasPassword = false, currentJob = null, currentBase = null, savingPass = false;

const STATE_LABEL = { valid: 'Сессия активна', expired: 'Нужен повторный вход', forbidden: 'Заблокировано MIREA', unavailable: 'Проверьте позже', invalidResponse: 'Проверьте позже' };
function notice(m) { $('notice').textContent = m || ''; }
function date(v) { return v ? new Date(v * 1000).toLocaleString() : 'неизвестно'; }

async function load() {
  try {
    const response = await fetch('/api/accounts/' + encodeURIComponent(accountId) + '/page', { cache: 'no-store' });
    if (response.status === 404) throw Error('Аккаунт не найден. Попросите свою ссылку заново.');
    if (!response.ok) throw Error('Не удалось загрузить страницу.');
    const data = await response.json();
    hasPassword = !!data.has_password;
    if (data.authed) { account = data; showAccount(); }
    else { account = null; showAuth(data); }
  } catch (error) { $('error').textContent = error.message; $('error').hidden = false; }
}

function showAuth(data) {
  $('error').hidden = true;
  $('account-card').hidden = true;
  $('auth-card').hidden = false;
  if (data.label) $('title').textContent = data.label;
  if (hasPassword) {
    $('auth-title').textContent = 'Вход';
    $('auth-hint').textContent = 'Введите пароль этой страницы.';
    $('auth-label').childNodes[0].textContent = 'Пароль';
    $('auth-password').autocomplete = 'current-password';
    $('auth-submit').textContent = 'Войти';
  } else {
    $('auth-title').textContent = 'Задайте пароль';
    $('auth-hint').textContent = 'Это первый вход — придумайте пароль, чтобы заходить сюда в следующий раз.';
    $('auth-label').childNodes[0].textContent = 'Новый пароль';
    $('auth-password').autocomplete = 'new-password';
    $('auth-submit').textContent = 'Сохранить и войти';
  }
}

function showAccount() {
  $('auth-card').hidden = true;
  $('error').hidden = true;
  $('account-card').hidden = false;
  $('title').textContent = account.label;
  $('label').textContent = account.label;
  const badge = $('state-badge');
  badge.textContent = STATE_LABEL[account.state] || account.state;
  badge.className = 'badge ' + account.state;
  $('meta').textContent = account.state === 'valid'
    ? `Продлевается автоматически · сессия до ${date(account.expires_at)}`
    : 'Нажмите «Продлить» и подтвердите вход в приложении MIREA.';
  $('renew').textContent = account.state === 'valid' ? 'Продлить сейчас' : 'Продлить';
  if (!savingPass) $('pass-allowed').checked = account.pass_allowed !== false;
}

async function auth(password) {
  const path = hasPassword ? 'login' : 'password';
  try {
    const response = await fetch('/api/accounts/' + encodeURIComponent(accountId) + '/page/' + path, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Не удалось войти.');
    $('auth-password').value = '';
    await load();
  } catch (error) { notice(error.message); }
}

async function savePass() {
  savingPass = true;
  const allowed = $('pass-allowed').checked;
  try {
    const response = await fetch('/api/accounts/' + encodeURIComponent(accountId) + '/pass-allowed', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ allowed, expected_version: account ? account.version : null }), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Не удалось сохранить.');
    account = result;
    notice(allowed ? 'Пропуск разрешён для приложения.' : 'Пропуск запрещён для приложения.');
  } catch (error) { notice(error.message); $('pass-allowed').checked = account && account.pass_allowed !== false; }
  finally { savingPass = false; }
}

async function startLogin() {
  notice('');
  $('login-card').hidden = false;
  $('login-status').textContent = 'Готовим безопасный вход…';
  $('qr').hidden = true; $('qr-link').hidden = true;
  $('login-card').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  try {
    const response = await fetch('/api/logins', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ label: account.label, account_id: accountId }), cache: 'no-store' });
    const result = await response.json();
    if (!response.ok) throw Error(typeof result.detail === 'string' ? result.detail : 'Не удалось начать вход.');
    currentBase = result.enrollment_path; currentJob = result.id;
    pollLogin();
  } catch (error) { $('login-status').textContent = error.message; }
}

async function pollLogin() {
  const job = currentJob, base = currentBase;
  if (!job) return;
  try {
    const response = await fetch(base + '/state', { cache: 'no-store' });
    if (!response.ok) throw Error('Вход истёк. Начните заново.');
    const result = await response.json();
    if (job !== currentJob) return;
    $('login-status').textContent = result.error || ({
      waiting: 'Ожидаем свободный слот…',
      login: 'Отсканируйте QR в приложении MIREA или откройте ссылку ниже.',
      finishing: 'Подтверждение получено. Завершаем сессию Pulse…',
      complete: 'Готово — аккаунт подключён и будет продлеваться автоматически.',
      expired: 'Вход истёк. Начните заново.', failed: 'Не удалось. Начните заново.', cancelled: 'Отменено.'
    }[result.status] || result.status);
    if (result.qr) { $('qr').src = base + '/qr?t=' + Date.now(); $('qr').hidden = false; $('qr-link').href = result.qr; $('qr-link').hidden = false; }
    else { $('qr').hidden = true; $('qr-link').hidden = true; }
    if (['complete', 'expired', 'failed', 'cancelled'].includes(result.status)) {
      currentJob = null;
      if (result.status === 'complete') { $('login-card').hidden = true; await load(); }
      return;
    }
  } catch (error) { $('login-status').textContent = error.message; currentJob = null; return; }
  if (currentJob === job) setTimeout(pollLogin, 2000);
}

$('auth-form').addEventListener('submit', e => { e.preventDefault(); const p = $('auth-password').value; if (p.length >= 4) auth(p); });
$('renew').onclick = () => startLogin();
$('pass-allowed').onchange = () => savePass();
$('close-login').onclick = () => { currentJob = null; $('login-card').hidden = true; };
$('logout').onclick = async () => {
  await fetch('/api/accounts/' + encodeURIComponent(accountId) + '/page/logout', { method: 'POST', cache: 'no-store' }).catch(() => {});
  account = null; await load();
};
$('pw-form').addEventListener('submit', async e => {
  e.preventDefault();
  const p = $('pw-new').value;
  if (p.length < 4) { notice('Пароль слишком короткий.'); return; }
  try {
    const response = await fetch('/api/accounts/' + encodeURIComponent(accountId) + '/page/password', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: p }), cache: 'no-store' });
    if (!response.ok) { const r = await response.json(); throw Error(typeof r.detail === 'string' ? r.detail : 'Ошибка.'); }
    $('pw-new').value = ''; notice('Пароль обновлён.');
  } catch (error) { notice(error.message); }
});

load();
setInterval(() => { if (account && !currentJob && !savingPass) load(); }, 10000);
