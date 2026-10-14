"""Optional Telegram bot: self-service renewal/enrollment and expiry alerts.

The bot adds no new trust: it drives the same in-process login and renewal the
panel and picker use, and it links a Telegram chat to an account only so it knows
where to send a "needs re-login" message. Enable it by setting TELEGRAM_TOKEN;
with the variable unset the server behaves exactly as before.
"""
import asyncio
import json
import time
from urllib.parse import urlparse

import httpx

POLL_TIMEOUT = 30
NOTIFY_INTERVAL = 60
NEEDS_LOGIN = ('expired', 'forbidden')

STATUS_TEXT = {
    'waiting': 'Preparing a secure login…',
    'login': 'Scan the QR above with the MIREA app, or open the link in a signed-in MIREA browser.',
    'finishing': 'Approval received. Confirming your Pulse session…',
}


class TelegramBot:
    def __init__(self, token, service, public_url='', http=None):
        self.token = token
        self.service = service
        self.base = 'https://api.telegram.org/bot' + token
        self.http = http or httpx.AsyncClient(timeout=httpx.Timeout(POLL_TIMEOUT + 10))
        self.offset = 0
        self.running = True
        self.pending = {}          # chat_id -> 'await_label'
        self.last_state = {}       # account_id -> last observed state (for alerts)
        parsed = urlparse(public_url or '')
        self.webapp_url = public_url if parsed.scheme in ('http', 'https') and parsed.hostname not in (None, '127.0.0.1', 'localhost') else ''

    # ---- Telegram API helpers -------------------------------------------------
    async def call(self, method, data=None, files=None, timeout=None):
        try:
            response = await self.http.post(f'{self.base}/{method}', data=data or {}, files=files,
                                             timeout=timeout or 20)
            payload = response.json()
            return payload.get('result') if payload.get('ok') else None
        except Exception:
            return None

    async def send(self, chat_id, text, keyboard=None):
        data = {'chat_id': chat_id, 'text': text, 'disable_web_page_preview': 'true'}
        if keyboard is not None:
            data['reply_markup'] = json.dumps({'inline_keyboard': keyboard})
        return await self.call('sendMessage', data)

    async def edit(self, chat_id, message_id, text, keyboard=None):
        if not message_id:
            return await self.send(chat_id, text, keyboard)
        data = {'chat_id': chat_id, 'message_id': message_id, 'text': text, 'disable_web_page_preview': 'true'}
        if keyboard is not None:
            data['reply_markup'] = json.dumps({'inline_keyboard': keyboard})
        return await self.call('editMessageText', data)

    async def send_photo(self, chat_id, png, caption=''):
        return await self.call('sendPhoto', data={'chat_id': chat_id, 'caption': caption[:1024]},
                               files={'photo': ('login.png', png, 'image/png')})

    async def answer(self, callback_id, text=None):
        data = {'callback_query_id': callback_id}
        if text:
            data['text'] = text
        return await self.call('answerCallbackQuery', data)

    # ---- Menus ----------------------------------------------------------------
    def main_keyboard(self):
        keyboard = [[{'text': '👤 My accounts', 'callback_data': 'mine'}],
                    [{'text': '➕ Add my account', 'callback_data': 'add'}]]
        if self.webapp_url:
            if self.webapp_url.startswith('https://'):
                keyboard.append([{'text': '🚀 Open mini-app', 'web_app': {'url': self.webapp_url}}])
            else:
                keyboard.append([{'text': '🌐 Open in browser', 'url': self.webapp_url}])
        return keyboard

    async def main_menu(self, chat_id):
        await self.send(chat_id,
            'Hi! I keep your MIREA session connected so you don’t have to re-scan the QR every time.\n\n'
            '• Pick your account to turn on alerts and renew it in seconds.\n'
            '• I’ll message you here the moment it needs a new login.',
            self.main_keyboard())

    async def list_accounts(self, chat_id):
        accounts = self.service.vault.accounts()
        if not accounts:
            await self.send(chat_id, 'No accounts yet. Add yours to get started.',
                            [[{'text': '➕ Add my account', 'callback_data': 'add'}]])
            return
        linked = set(self.service.vault.telegram_accounts(chat_id))
        rows = []
        for account in accounts:
            mark = '✅' if account['state'] == 'valid' else '⚠️'
            bell = ' 🔔' if account['id'] in linked else ''
            rows.append([{'text': f'{mark} {account["label"]}{bell}', 'callback_data': 'pick:' + account['id']}])
        rows.append([{'text': '➕ Add', 'callback_data': 'add'}, {'text': '🔄 Refresh', 'callback_data': 'mine'}])
        await self.send(chat_id, 'Tap your account to renew it or turn on alerts (🔔 = alerts on):', rows)

    async def show_account(self, chat_id, account_id):
        account = self.service.vault.account(account_id)
        if account is None:
            await self.send(chat_id, 'That account no longer exists. Send /menu to start over.')
            return
        self.service.vault.link_telegram(chat_id, account_id)
        allowed = account.get('pass_allowed', True)
        if account['state'] == 'valid':
            line = f'“{account["label"]}” is connected and renews automatically.\nAlerts are on — I’ll message you if it ever needs a new login.'
        else:
            line = f'“{account["label"]}” needs a new login. Tap Renew and confirm in the MIREA app.'
        line += '\nPass in app: ' + ('ON' if allowed else 'OFF')
        keyboard = [
            [{'text': '🔄 Renew now', 'callback_data': 'renew:' + account_id}],
            [{'text': ('🚫 Disable pass in app' if allowed else '🪪 Enable pass in app'),
              'callback_data': f'pass:{account_id}:{0 if allowed else 1}'}],
            [{'text': '🔕 Stop alerts', 'callback_data': 'unlink:' + account_id}],
            [{'text': '⬅️ Back', 'callback_data': 'mine'}]]
        if self.webapp_url:
            page = self.webapp_url.rstrip('/') + '/u/' + account_id
            keyboard.insert(1, [{'text': '🌐 My page', 'web_app': {'url': page}} if page.startswith('https://')
                                else {'text': '🌐 My page', 'url': page}])
        await self.send(chat_id, line, keyboard)

    # ---- Update handling ------------------------------------------------------
    async def handle_update(self, update):
        if 'callback_query' in update:
            await self.on_callback(update['callback_query'])
        elif 'message' in update:
            await self.on_message(update['message'])

    async def on_message(self, message):
        chat = message.get('chat') or {}
        chat_id = chat.get('id')
        if chat_id is None or chat.get('type') != 'private':
            return
        text = (message.get('text') or '').strip()
        if text.startswith('/start') or text.startswith('/menu'):
            self.pending.pop(chat_id, None)
            await self.main_menu(chat_id)
            return
        if self.pending.get(chat_id) == 'await_label':
            self.pending.pop(chat_id, None)
            if text.startswith('/'):
                await self.main_menu(chat_id)
                return
            await self.start_renewal(chat_id, None, text[:60] or 'Account')
            return
        await self.main_menu(chat_id)

    async def on_callback(self, query):
        await self.answer(query.get('id'))
        chat_id = ((query.get('message') or {}).get('chat') or {}).get('id')
        data = query.get('data') or ''
        if chat_id is None:
            return
        if data == 'mine':
            await self.list_accounts(chat_id)
        elif data == 'add':
            self.pending[chat_id] = 'await_label'
            await self.send(chat_id, 'Send me a short name or group label for the new account (e.g. “Ivan · ИНБО-10-23”).')
        elif data.startswith('pick:'):
            await self.show_account(chat_id, data[5:])
        elif data.startswith('renew:'):
            account = self.service.vault.account(data[6:])
            if account is None:
                await self.send(chat_id, 'That account no longer exists. Send /menu to start over.')
            else:
                await self.start_renewal(chat_id, account['id'], account['label'])
        elif data.startswith('pass:'):
            parts = data.split(':', 2)
            if len(parts) == 3:
                try:
                    self.service.vault.set_pass_allowed(parts[1], parts[2] == '1')
                    await self.service.changed()
                except KeyError:
                    pass
            await self.show_account(chat_id, parts[1] if len(parts) == 3 else '')
        elif data.startswith('unlink:'):
            self.service.vault.unlink_telegram(chat_id, data[7:])
            await self.send(chat_id, '🔕 Alerts off for that account.')
            await self.list_accounts(chat_id)
        else:
            await self.main_menu(chat_id)

    async def start_renewal(self, chat_id, account_id, label):
        try:
            job, _token = self.service.browser.start(label, account_id)
        except ValueError as error:
            await self.send(chat_id, '⚠️ ' + str(error))
            return
        status = await self.send(chat_id, 'Preparing a secure login…')
        message_id = (status or {}).get('message_id')
        qr_sent, last_text = False, None
        deadline = time.time() + 600
        while job.status in ('waiting', 'login', 'finishing') and time.time() < deadline:
            if job.qr and not qr_sent:
                png = self.service.browser.qr_png(job)
                if png:
                    await self.send_photo(chat_id, png, 'Scan this in the MIREA app, or open:\n' + (job.qr or ''))
                    qr_sent = True
            text = STATUS_TEXT.get(job.status, 'Working…')
            if text != last_text:
                await self.edit(chat_id, message_id, text)
                last_text = text
            await asyncio.sleep(2)
        if job.status == 'complete':
            linked = job.result_id or account_id
            if linked:
                try:
                    self.service.vault.link_telegram(chat_id, linked)
                except KeyError:
                    pass
                self.last_state[linked] = 'valid'
            await self.edit(chat_id, message_id,
                '✅ Connected! Your account will renew automatically from now on. '
                'I’ll message you here if it ever needs another login.')
        else:
            await self.edit(chat_id, message_id, '⚠️ ' + (job.error or 'Login did not complete. Try again from /menu.'))

    # ---- Background loops ------------------------------------------------------
    async def notify_scan(self):
        for account in self.service.vault.accounts():
            previous = self.last_state.get(account['id'], '__new__')
            self.last_state[account['id']] = account['state']
            # Only alert on a transition observed while running, so a restart
            # does not re-announce accounts that were already expired.
            if previous == '__new__':
                continue
            if account['state'] in NEEDS_LOGIN and previous not in NEEDS_LOGIN:
                for chat_id in self.service.vault.telegram_chats(account['id']):
                    await self.send(chat_id,
                        f'⚠️ Your MIREA session “{account["label"]}” needs a new login.',
                        [[{'text': '🔄 Renew now', 'callback_data': 'renew:' + account['id']}]])

    async def notify_loop(self):
        while self.running:
            try:
                await self.notify_scan()
            except asyncio.CancelledError:
                raise
            except Exception:
                pass
            await asyncio.sleep(NOTIFY_INTERVAL)

    async def poll_loop(self):
        await self.call('deleteWebhook', {'drop_pending_updates': 'false'})
        await self.call('setMyCommands', {'commands': json.dumps([
            {'command': 'menu', 'description': 'Manage and renew your MIREA account'}])})
        while self.running:
            result = await self.call('getUpdates', {'offset': self.offset, 'timeout': POLL_TIMEOUT,
                'allowed_updates': json.dumps(['message', 'callback_query'])}, timeout=POLL_TIMEOUT + 10)
            if not result:
                await asyncio.sleep(3)
                continue
            for update in result:
                self.offset = max(self.offset, update.get('update_id', -1) + 1)
                try:
                    await self.handle_update(update)
                except asyncio.CancelledError:
                    raise
                except Exception:
                    pass

    async def run(self):
        try:
            await asyncio.gather(self.poll_loop(), self.notify_loop())
        except asyncio.CancelledError:
            raise

    async def close(self):
        self.running = False
        await self.http.aclose()
