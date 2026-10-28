'use strict';
(async () => {
  const status = document.getElementById('status'), open = document.getElementById('open');
  try {
    const response = await fetch('/site/telegram-link', { method: 'POST', cache: 'no-store', credentials: 'same-origin' });
    if (response.status === 401) { location.assign('/auth/aaid?next=/tg/connect'); return; }
    const data = await response.json().catch(() => ({}));
    if (!response.ok) throw Error(typeof data.detail === 'string' ? data.detail : 'Сервер недоступен.');
    open.href = data.url; open.hidden = false;
    status.textContent = 'Открываем Telegram… Если ничего не произошло, нажмите кнопку ниже.';
    location.assign(data.url);
  } catch (error) { status.textContent = error.message; }
})();
