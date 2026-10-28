"""Optional Telegram bot: AA ID sign-in, the person's own account, pass switch and grants,
MIREA re-login and expiry alerts, in Russian or English.

The bot adds no new trust: it drives the same in-process login and renewal the
panel and picker use, and it links a Telegram chat to an account only so it knows
where to send a "needs re-login" message. Enable it by setting TELEGRAM_TOKEN;
with the variable unset the server behaves exactly as before.
"""
import asyncio
import json
import logging
import time
from urllib.parse import urlparse

import httpx

logger = logging.getLogger(__name__)

POLL_TIMEOUT = 30
NOTIFY_INTERVAL = 60
NEEDS_LOGIN = ('expired', 'forbidden')
LANGUAGES = ('ru', 'en')

TEXT = {
    'ru': {
        'hello': 'Привет! Я бот AntiAttendance.\n\nВойдите через AA ID, и я привяжу этот Telegram к вашему аккаунту: буду показывать статус сессии МИРЭА, управлять пропуском и сразу сообщу, если понадобится новый вход.',
        'sign_in': '🔑 Войти через AA ID',
        'no_site': 'Вход через AA ID недоступен: у сервера не задан PUBLIC_URL.',
        'settings': '⚙️ Настройки',
        'open_site': '🌐 Открыть сайт',
        'today': '📋 Отметки за сегодня',
        'renew': '🔄 Обновить вход МИРЭА',
        'pass_on': '🪪 Включить пропуск',
        'pass_off': '🚫 Выключить пропуск',
        'refresh': '🔄 Обновить',
        'back': '⬅️ Назад',
        'card': '👤 {label}{group}\nAA ID{username}\n\nСессия МИРЭА: {state}\nПропуск в приложении: {passes}\nУведомления: {alerts}',
        'state_valid': '✅ работает и продлевается сама',
        'state_bad': '⚠️ нужен новый вход',
        'on': 'включён', 'off': 'выключен', 'alerts_on': 'включены', 'alerts_off': 'выключены',
        'linked': '✅ Telegram привязан к вашему AA ID.',
        'link_bad': 'Ссылка устарела или уже использована. Откройте вход через AA ID ещё раз.',
        'link_gone': 'Аккаунт, к которому был привязан бот, удалён. Войдите через AA ID заново.',
        'settings_text': '⚙️ Настройки\n\nЯзык: {lang}\nУведомления о сессии: {alerts}',
        'lang_name': 'Русский',
        'alerts_toggle_on': '🔔 Включить уведомления', 'alerts_toggle_off': '🔕 Выключить уведомления',
        'unlink': '🚪 Отвязать AA ID',
        'unlink_confirm': 'Отвязать этот Telegram от AA ID? Бот перестанет показывать ваш аккаунт и присылать уведомления.',
        'unlink_yes': 'Да, отвязать', 'cancel': 'Отмена',
        'unlinked': 'Telegram отвязан от AA ID.',
        'pass_confirm_on': 'Включить пропуск в приложении? Им смогут пользоваться вы и те, кому вы разрешили.',
        'pass_confirm_off': 'Выключить пропуск? Его не сможет открыть никто, даже те, кому вы разрешили.',
        'yes': 'Да', 'not_owner': 'Это не ваш аккаунт. Войдите через AA ID, чтобы управлять своим.',
        'gone': 'Аккаунт не найден. Отправьте /menu.',
        'busy': 'Сервер занят входами. Повторите попытку позже.',
        'login_failed': 'Не удалось завершить вход. Повторите попытку из /menu.',
        'preparing': 'Готовим безопасный вход…',
        'scan': 'Отсканируйте QR в приложении МИРЭА или откройте ссылку в браузере, где вы вошли в МИРЭА.',
        'finishing': 'Подтверждение получено, проверяем сессию Пульса…',
        'working': 'Работаем…',
        'qr_caption': 'Отсканируйте в приложении МИРЭА или откройте:\n',
        'connected': '✅ Готово! Сессия будет продлеваться сама. Если понадобится новый вход, я напишу сюда.',
        'login_incomplete': 'Вход не завершён. Повторите попытку из /menu.',
        'needs_login': '⚠️ Сессия МИРЭА «{label}» требует нового входа.',
        'request_prompt': 'Запросить доступ к пропуску «{label}» для приложения, из которого вы открыли эту ссылку? Владелец увидит ваш Telegram и сможет отозвать доступ. Подтверждайте только свои запросы.',
        'request_button': 'Запросить доступ',
        'request_expired': 'Ссылка истекла или уже использована. Создайте новый запрос в приложении.',
        'request_approved': 'Доступ уже разрешён. Вернитесь в приложение и обновите пропуска.',
        'request_created': 'Запрос создан. Ожидайте решения владельца; затем вернитесь в приложение и обновите пропуска.',
        'request_invalid': 'Запрос недействителен, уже использован или связан с другим Telegram. Создайте новый запрос в приложении.',
        'request_owner': 'Запрос доступа к вашему пропуску «{label}».\nКто: {who}\nРазрешение действует для этого человека, пока вы его не отзовёте.',
        'allow': 'Разрешить', 'deny': 'Отклонить',
        'allowed': 'Доступ разрешён.', 'denied': 'Запрос отклонён.',
        'decided': 'Решение уже принято, запрос истёк или вы не владелец этого пропуска.',
        'revoke': 'Отозвать: {label}', 'revoked': 'Доступ отозван.', 'revoke_missing': 'Разрешение не найдено или вы не владелец.',
        'command': 'Мой аккаунт и настройки',
    },
    'en': {
        'hello': 'Hi! I am the AntiAttendance bot.\n\nSign in with AA ID and I will link this Telegram to your account: I show your MIREA session status, manage your pass, and message you as soon as a new login is needed.',
        'sign_in': '🔑 Sign in with AA ID',
        'no_site': 'AA ID sign-in is unavailable: the server has no PUBLIC_URL.',
        'settings': '⚙️ Settings',
        'open_site': '🌐 Open the site',
        'today': '📋 Today\'s marks',
        'renew': '🔄 Renew MIREA login',
        'pass_on': '🪪 Turn the pass on',
        'pass_off': '🚫 Turn the pass off',
        'refresh': '🔄 Refresh',
        'back': '⬅️ Back',
        'card': '👤 {label}{group}\nAA ID{username}\n\nMIREA session: {state}\nPass in the app: {passes}\nAlerts: {alerts}',
        'state_valid': '✅ working, renews itself',
        'state_bad': '⚠️ needs a new login',
        'on': 'on', 'off': 'off', 'alerts_on': 'on', 'alerts_off': 'off',
        'linked': '✅ This Telegram is now linked to your AA ID.',
        'link_bad': 'This link has expired or was already used. Start the AA ID sign-in again.',
        'link_gone': 'The account this bot was linked to was deleted. Sign in with AA ID again.',
        'settings_text': '⚙️ Settings\n\nLanguage: {lang}\nSession alerts: {alerts}',
        'lang_name': 'English',
        'alerts_toggle_on': '🔔 Turn alerts on', 'alerts_toggle_off': '🔕 Turn alerts off',
        'unlink': '🚪 Unlink AA ID',
        'unlink_confirm': 'Unlink this Telegram from AA ID? The bot will stop showing your account and sending alerts.',
        'unlink_yes': 'Yes, unlink', 'cancel': 'Cancel',
        'unlinked': 'This Telegram is unlinked from AA ID.',
        'pass_confirm_on': 'Turn the pass on in the app? You and the people you allowed will be able to use it.',
        'pass_confirm_off': 'Turn the pass off? Nobody will be able to open it, not even the people you allowed.',
        'yes': 'Yes', 'not_owner': 'This is not your account. Sign in with AA ID to manage yours.',
        'gone': 'Account not found. Send /menu.',
        'busy': 'The server is busy with logins. Try again later.',
        'login_failed': 'Could not finish the login. Try again from /menu.',
        'preparing': 'Preparing a secure login…',
        'scan': 'Scan the QR with the MIREA app, or open the link in a browser signed in to MIREA.',
        'finishing': 'Approval received. Checking your Pulse session…',
        'working': 'Working…',
        'qr_caption': 'Scan this in the MIREA app, or open:\n',
        'connected': '✅ Done! The session will renew itself. If it ever needs another login, I will message you here.',
        'login_incomplete': 'The login did not complete. Try again from /menu.',
        'needs_login': '⚠️ Your MIREA session “{label}” needs a new login.',
        'request_prompt': 'Request access to the pass of “{label}” for the app you opened this link from? The owner will see your Telegram and can revoke access. Only confirm your own requests.',
        'request_button': 'Request access',
        'request_expired': 'This link has expired or was already used. Make a new request in the app.',
        'request_approved': 'Access is already allowed. Go back to the app and refresh the passes.',
        'request_created': 'Request sent. Wait for the owner\'s decision, then go back to the app and refresh the passes.',
        'request_invalid': 'The request is invalid, already used, or tied to another Telegram. Make a new request in the app.',
        'request_owner': 'Someone asks for access to your pass “{label}”.\nWho: {who}\nThe permission lasts for this person until you revoke it.',
        'allow': 'Allow', 'deny': 'Deny',
        'allowed': 'Access allowed.', 'denied': 'Request denied.',
        'decided': 'Already decided, expired, or you are not the owner of this pass.',
        'revoke': 'Revoke: {label}', 'revoked': 'Access revoked.', 'revoke_missing': 'Permission not found, or you are not the owner.',
        'command': 'My account and settings',
    },
}
STATUS_KEYS = {'waiting': 'preparing', 'login': 'scan', 'finishing': 'finishing'}


class TelegramBot:
    def __init__(self, token, service, public_url='', http=None, relay_url='', relay_key='', proxy_url='', proxy_key=''):
        self.token = token
        self.service = service
        parsed_relay = urlparse(relay_url)
        if bool(relay_url) != bool(relay_key) or (relay_url and (
            parsed_relay.scheme != 'https' or not parsed_relay.hostname or parsed_relay.username or
            parsed_relay.password or parsed_relay.query or parsed_relay.fragment or len(relay_key) < 32)):
            raise ValueError('Set a valid HTTPS TELEGRAM_RELAY_URL and TELEGRAM_RELAY_KEY together')
        parsed_proxy = urlparse(proxy_url)
        if bool(proxy_url) != bool(proxy_key) or (proxy_url and (
            parsed_proxy.scheme != 'https' or not parsed_proxy.hostname or parsed_proxy.username or
            parsed_proxy.password or parsed_proxy.query or parsed_proxy.fragment or parsed_proxy.path not in ('', '/') or
            len(proxy_key) < 16 or not all(char.isascii() and (char.isalnum() or char in '_-') for char in proxy_key))):
            raise ValueError('Set a valid HTTPS TELEGRAM_PROXY_URL and TELEGRAM_PROXY_KEY together')
        if relay_url and proxy_url:
            raise ValueError('Configure only one Telegram relay or proxy')
        self.base = (relay_url.rstrip('/') if relay_url else
                     f'{proxy_url.rstrip("/")}/{proxy_key}/bot{token}' if proxy_url else
                     'https://api.telegram.org/bot' + token)
        self.relay_key = relay_key
        self.proxy_key = proxy_key
        self.http = http or httpx.AsyncClient(timeout=httpx.Timeout(20), limits=httpx.Limits(max_keepalive_connections=20, keepalive_expiry=60))
        # The long poll holds its connection for up to 30 s, so it gets its own client
        # and replies never queue behind it.
        self.poll_http = http or httpx.AsyncClient(timeout=httpx.Timeout(POLL_TIMEOUT + 10))
        self.update_tasks = set()
        self.chat_locks = {}
        self.username = ''
        self.offset = 0
        self.running = True
        self.login_tasks = set()
        self.last_state = {}       # account_id -> last observed state (for alerts)
        parsed = urlparse(public_url or '')
        self.webapp_url = public_url if parsed.scheme in ('http', 'https') and parsed.hostname not in (None, '127.0.0.1', 'localhost') else ''

    # ---- Telegram API helpers -------------------------------------------------
    async def call(self, method, data=None, files=None, timeout=None, client=None):
        try:
            response = await (client or self.http).post(f'{self.base}/{method}', data=data or {}, files=files,
                                             headers={'Authorization': 'Bearer ' + self.relay_key} if self.relay_key else {},
                                             timeout=timeout or 20)
            try:
                payload = response.json()
            except (ValueError, json.JSONDecodeError):
                detail = response.text[:200]
                for secret in (self.token, self.relay_key, self.proxy_key):
                    if secret:
                        detail = detail.replace(secret, '[redacted]')
                logger.warning('Telegram API %s returned non-JSON HTTP %s (%s): %s',
                               method, response.status_code, response.headers.get('content-type', 'unknown'), detail)
                return None
            if not isinstance(payload, dict) or not payload.get('ok'):
                description = str(payload.get('description', 'unspecified API error')) if isinstance(payload, dict) else 'invalid response'
                for secret in (self.token, self.relay_key, self.proxy_key):
                    if secret:
                        description = description.replace(secret, '[redacted]')
                logger.warning('Telegram API %s failed (HTTP %s): %s', method, response.status_code, description[:240])
                return None
            return payload.get('result')
        except Exception as error:
            logger.warning('Telegram API %s request failed (%s)', method, type(error).__name__)
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

    # ---- Language and linked person -------------------------------------------
    def lang(self, chat_id):
        lang = self.service.vault.telegram_user(chat_id)['lang']
        return lang if lang in LANGUAGES else 'ru'

    def t(self, chat_id, key, **values):
        text = TEXT[self.lang(chat_id)][key]
        return text.format(**values) if values else text

    def linked_account(self, chat_id):
        user = self.service.vault.telegram_user(chat_id)
        return self.service.vault.account(user['account_id']) if user['account_id'] else None

    def site_button(self, chat_id, key, path):
        url = self.webapp_url.rstrip('/') + path
        return {'text': self.t(chat_id, key), 'web_app': {'url': url}} if url.startswith('https://') else {'text': self.t(chat_id, key), 'url': url}

    # ---- Menus ----------------------------------------------------------------
    async def main_menu(self, chat_id):
        account = self.linked_account(chat_id)
        if account is not None:
            await self.show_account(chat_id, account['id'])
            return
        keyboard = []
        if self.webapp_url:
            url = self.webapp_url.rstrip('/') + '/tg/connect'
            keyboard.append([{'text': self.t(chat_id, 'sign_in'), 'url': url}])
        keyboard.append([{'text': self.t(chat_id, 'settings'), 'callback_data': 'settings'}])
        await self.send(chat_id, self.t(chat_id, 'hello') + ('' if self.webapp_url else '\n\n' + self.t(chat_id, 'no_site')), keyboard)

    async def show_account(self, chat_id, account_id):
        vault = self.service.vault
        account = vault.account(account_id)
        if account is None:
            await self.send(chat_id, self.t(chat_id, 'gone'))
            return
        if not self.service.agreements.owns(chat_id, account_id):
            # Seeing or managing an account needs proof: the AA ID link or a MIREA login.
            keyboard = [[{'text': self.t(chat_id, 'sign_in'), 'url': self.webapp_url.rstrip('/') + '/tg/connect'}]] if self.webapp_url else []
            await self.send(chat_id, self.t(chat_id, 'not_owner'), keyboard)
            return
        user = vault.telegram_user(chat_id)
        allowed = bool(account['pass_allowed'])
        text = self.t(chat_id, 'card', label=account['label'],
                      group=' · ' + account['group_name'] if account['group_name'] else '',
                      username=' @' + account['username'] if account['username'] else '',
                      state=self.t(chat_id, 'state_valid' if account['state'] == 'valid' else 'state_bad'),
                      passes=self.t(chat_id, 'on' if allowed else 'off'),
                      alerts=self.t(chat_id, 'alerts_on' if user['alerts'] else 'alerts_off'))
        keyboard = [[{'text': self.t(chat_id, 'renew'), 'callback_data': 'renew:' + account_id}],
                    [{'text': self.t(chat_id, 'pass_off' if allowed else 'pass_on'), 'callback_data': f'pass:{account_id}:{0 if allowed else 1}'}]]
        for grant in self.service.agreements.grants(chat_id, account_id):
            keyboard.append([{'text': self.t(chat_id, 'revoke', label=grant['label'][:45]), 'callback_data': 'revoke:' + grant['id']}])
        if self.webapp_url:
            keyboard.append([self.site_button(chat_id, 'today', '/today'), self.site_button(chat_id, 'open_site', '/')])
        keyboard.append([{'text': self.t(chat_id, 'settings'), 'callback_data': 'settings'},
                         {'text': self.t(chat_id, 'refresh'), 'callback_data': 'home'}])
        await self.send(chat_id, text, keyboard)

    async def settings(self, chat_id, message_id=None):
        user = self.service.vault.telegram_user(chat_id)
        keyboard = [[{'text': ('• ' if user['lang'] == 'ru' else '') + 'Русский', 'callback_data': 'lang:ru'},
                     {'text': ('• ' if user['lang'] == 'en' else '') + 'English', 'callback_data': 'lang:en'}]]
        if user['account_id']:
            keyboard.append([{'text': self.t(chat_id, 'alerts_toggle_off' if user['alerts'] else 'alerts_toggle_on'),
                              'callback_data': f'alerts:{0 if user["alerts"] else 1}'}])
            keyboard.append([{'text': self.t(chat_id, 'unlink'), 'callback_data': 'logout'}])
        keyboard.append([{'text': self.t(chat_id, 'back'), 'callback_data': 'home'}])
        text = self.t(chat_id, 'settings_text', lang=self.t(chat_id, 'lang_name'),
                      alerts=self.t(chat_id, 'alerts_on' if user['alerts'] else 'alerts_off'))
        await self.edit(chat_id, message_id, text, keyboard)

    async def link_aaid(self, chat_id, code):
        vault = self.service.vault
        claimed = vault.claim_telegram_code(code)
        account = vault.account(claimed['account_id']) if claimed else None
        if account is None:
            await self.send(chat_id, self.t(chat_id, 'link_bad' if claimed is None else 'link_gone'))
            return
        previous = vault.telegram_user(chat_id)['account_id']
        if previous and previous != account['id']:
            self.service.agreements.unlink_owner(chat_id, previous)
        vault.update_telegram_user(chat_id, aaid_id=claimed['aaid_id'], account_id=account['id'], linked=time.time())
        # The AA ID is bound to this MIREA account, which proves ownership as a MIREA login would.
        self.service.agreements.verify_owner(chat_id, account['id'])
        vault.audit('telegram_aaid_linked', 'aaid:' + claimed['aaid_id'], account['id'])
        self.last_state[account['id']] = account['state']
        await self.service.changed()
        await self.send(chat_id, self.t(chat_id, 'linked'))
        await self.show_account(chat_id, account['id'])

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
        if text.startswith('/start aaid_'):
            await self.link_aaid(chat_id, text.split(' ', 1)[1][5:][:64])
            return
        if text.startswith('/start access_'):
            identifier = text.split(' ', 1)[1][7:]
            pending = self.service.agreements.preview(identifier)
            account = self.service.vault.account(pending['account_id']) if pending else None
            if account:
                await self.send(chat_id, self.t(chat_id, 'request_prompt', label=account['label']),
                                [[{'text': self.t(chat_id, 'request_button'), 'callback_data': 'request:' + identifier}]])
            else:
                await self.send(chat_id, self.t(chat_id, 'request_expired'))
            return
        if text.startswith('/settings'):
            await self.settings(chat_id)
            return
        await self.main_menu(chat_id)

    async def on_callback(self, query):
        chat = ((query.get('message') or {}).get('chat') or {})
        chat_id = chat.get('id')
        message_id = (query.get('message') or {}).get('message_id')
        data = query.get('data') or ''
        sender = query.get('from') or {}
        # The spinner on the button stops at once; the reply is sent alongside.
        answered = asyncio.ensure_future(self.answer(query.get('id')))
        try:
            if chat_id is None or chat.get('type') != 'private' or sender.get('id') != chat_id:
                return
            await self.on_action(chat_id, message_id, data, sender)
        finally:
            await answered

    async def on_action(self, chat_id, message_id, data, sender):
        agreements, vault = self.service.agreements, self.service.vault
        if data.startswith('request:'):
            name = ' '.join(str(sender.get(part) or '') for part in ('first_name', 'last_name')).strip()
            username = sender.get('username')
            label = f'{name} ' + (f'(@{username}) ' if username else '') + f'· Telegram ID {chat_id}'
            state = agreements.claim(data[8:], chat_id, label)
            if state:
                await self.service.changed()
                await self.send(chat_id, self.t(chat_id, 'request_approved' if state == 'approved' else 'request_created'))
                await self.send_requests()
            else:
                await self.send(chat_id, self.t(chat_id, 'request_invalid'))
        elif data.startswith('agree:'):
            parts = data.split(':')
            if len(parts) == 3 and parts[2] in ('0', '1') and agreements.decide(parts[1], chat_id, parts[2] == '1'):
                await self.service.changed()
                await self.edit(chat_id, message_id, self.t(chat_id, 'allowed' if parts[2] == '1' else 'denied'), [])
            else:
                await self.send(chat_id, self.t(chat_id, 'decided'))
        elif data.startswith('revoke:'):
            if agreements.revoke(data[7:], chat_id):
                await self.service.changed()
                await self.send(chat_id, self.t(chat_id, 'revoked'))
            else:
                await self.send(chat_id, self.t(chat_id, 'revoke_missing'))
        elif data.startswith('pick:'):
            await self.show_account(chat_id, data[5:])
        elif data.startswith('renew:'):
            account = vault.account(data[6:])
            if account is None:
                await self.send(chat_id, self.t(chat_id, 'gone'))
            elif not agreements.owns(chat_id, account['id']):
                await self.show_account(chat_id, account['id'])
            else:
                await self.queue_login(chat_id, account['id'], account['label'])
        elif data.startswith('pass:') or data.startswith('passok:'):
            parts = data.split(':', 2)
            if len(parts) != 3 or parts[2] not in ('0', '1') or not agreements.owns(chat_id, parts[1]):
                await self.show_account(chat_id, parts[1] if len(parts) == 3 else '')
            elif parts[0] == 'pass':
                # Every change of the pass switch is confirmed, as on the site.
                await self.edit(chat_id, message_id, self.t(chat_id, 'pass_confirm_on' if parts[2] == '1' else 'pass_confirm_off'),
                                [[{'text': self.t(chat_id, 'yes'), 'callback_data': f'passok:{parts[1]}:{parts[2]}'},
                                  {'text': self.t(chat_id, 'cancel'), 'callback_data': 'home'}]])
            else:
                try:
                    vault.set_pass_allowed(parts[1], parts[2] == '1')
                    vault.audit('pass_allowed_' + ('on' if parts[2] == '1' else 'off'), 'telegram:' + str(chat_id), parts[1])
                    await self.service.changed()
                except KeyError:
                    pass
                await self.show_account(chat_id, parts[1])
        elif data.startswith('unlink:'):
            agreements.unlink_owner(chat_id, data[7:])
            await self.service.changed()
            await self.main_menu(chat_id)
        elif data == 'settings':
            await self.settings(chat_id, message_id)
        elif data.startswith('lang:') and data[5:] in LANGUAGES:
            vault.update_telegram_user(chat_id, lang=data[5:])
            await self.call('setMyCommands', {'commands': json.dumps([{'command': 'menu', 'description': self.t(chat_id, 'command')}]),
                                              'scope': json.dumps({'type': 'chat', 'chat_id': chat_id})})
            await self.settings(chat_id, message_id)
        elif data in ('alerts:0', 'alerts:1'):
            vault.update_telegram_user(chat_id, alerts=int(data[-1]))
            await self.settings(chat_id, message_id)
        elif data == 'logout':
            await self.edit(chat_id, message_id, self.t(chat_id, 'unlink_confirm'),
                            [[{'text': self.t(chat_id, 'unlink_yes'), 'callback_data': 'logoutok'},
                              {'text': self.t(chat_id, 'cancel'), 'callback_data': 'settings'}]])
        elif data == 'logoutok':
            account_id = vault.telegram_user(chat_id)['account_id']
            if account_id:
                agreements.unlink_owner(chat_id, account_id)
                vault.audit('telegram_aaid_unlinked', 'telegram:' + str(chat_id), account_id)
            vault.update_telegram_user(chat_id, aaid_id=None, account_id=None, linked=None)
            await self.service.changed()
            await self.edit(chat_id, message_id, self.t(chat_id, 'unlinked'), [])
            await self.main_menu(chat_id)
        else:
            await self.main_menu(chat_id)

    async def queue_login(self, chat_id, account_id, label):
        if len(self.login_tasks) >= 24:
            await self.send(chat_id, self.t(chat_id, 'busy'))
            return
        async def work():
            try:
                await self.start_renewal(chat_id, account_id, label)
            except asyncio.CancelledError:
                raise
            except Exception:
                await self.send(chat_id, self.t(chat_id, 'login_failed'))
        task = asyncio.create_task(work())
        self.login_tasks.add(task)
        task.add_done_callback(self.login_tasks.discard)

    async def start_renewal(self, chat_id, account_id, label):
        try:
            job, _token = self.service.browser.start(label, account_id)
        except ValueError as error:
            await self.send(chat_id, '⚠️ ' + str(error))
            return
        status = await self.send(chat_id, self.t(chat_id, 'preparing'))
        message_id = (status or {}).get('message_id')
        qr_sent, last_text = False, None
        deadline = time.time() + 600
        while job.status in ('waiting', 'login', 'finishing') and time.time() < deadline:
            if job.qr and not qr_sent:
                png = self.service.browser.qr_png(job)
                if png:
                    await self.send_photo(chat_id, png, self.t(chat_id, 'qr_caption') + (job.qr or ''))
                    qr_sent = True
            text = self.t(chat_id, STATUS_KEYS.get(job.status, 'working'))
            if text != last_text:
                await self.edit(chat_id, message_id, text)
                last_text = text
            await asyncio.sleep(1)
        if job.status == 'complete':
            linked = job.result_id or account_id
            if linked:
                try:
                    self.service.agreements.verify_owner(chat_id, linked)
                except KeyError:
                    pass
                self.last_state[linked] = 'valid'
                await self.send_requests()
            await self.edit(chat_id, message_id, self.t(chat_id, 'connected'))
        else:
            await self.edit(chat_id, message_id, '⚠️ ' + (job.error or self.t(chat_id, 'login_incomplete')))

    # ---- Background loops ------------------------------------------------------
    async def send_requests(self):
        for request in self.service.agreements.notifications():
            account = self.service.vault.account(request['account_id'])
            if not account:
                continue
            delivered = False
            for owner in self.service.agreements.owners(account['id']):
                result = await self.send(owner, self.t(owner, 'request_owner', label=account['label'], who=request['label']),
                                         [[{'text': self.t(owner, 'allow'), 'callback_data': 'agree:' + request['id'] + ':1'},
                                           {'text': self.t(owner, 'deny'), 'callback_data': 'agree:' + request['id'] + ':0'}]])
                delivered = delivered or result is not None
            if delivered:
                self.service.agreements.notified(request['id'])

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
                    if not self.service.vault.telegram_user(chat_id)['alerts']:
                        continue
                    await self.send(chat_id, self.t(chat_id, 'needs_login', label=account['label']),
                                    [[{'text': self.t(chat_id, 'renew'), 'callback_data': 'renew:' + account['id']}]])

    async def notify_loop(self):
        while self.running:
            try:
                await self.notify_scan()
                await self.send_requests()
            except asyncio.CancelledError:
                raise
            except Exception as error:
                logger.exception('Telegram notification loop failed (%s)', type(error).__name__)
            await asyncio.sleep(NOTIFY_INTERVAL)

    async def dispatch(self, update):
        # Each chat's updates run in order, but chats never wait for each other and
        # polling never waits for a handler (each handler makes several API calls).
        source = update.get('callback_query', {}).get('from') or (update.get('message') or {}).get('chat') or {}
        lock = self.chat_locks.setdefault(source.get('id'), asyncio.Lock())
        async with lock:
            try:
                await self.handle_update(update)
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception('Telegram update failed')

    async def poll_loop(self):
        me = await self.call('getMe')
        if me and isinstance(me.get('username'), str):
            self.username = me['username']
            logger.info('Telegram bot @%s connected; polling updates', self.username)
        await self.call('deleteWebhook', {'drop_pending_updates': 'false'})
        await self.call('setMyCommands', {'commands': json.dumps([{'command': 'menu', 'description': TEXT['ru']['command']}])})
        await self.call('setMyCommands', {'commands': json.dumps([{'command': 'menu', 'description': TEXT['en']['command']}]), 'language_code': 'en'})
        while self.running:
            if not self.username:
                me = await self.call('getMe')
                if me and isinstance(me.get('username'), str):
                    self.username = me['username']
            started = time.monotonic()
            result = await self.call('getUpdates', {'offset': self.offset, 'timeout': POLL_TIMEOUT,
                'allowed_updates': json.dumps(['message', 'callback_query'])}, timeout=POLL_TIMEOUT + 10, client=self.poll_http)
            if result is None:
                # Only a failed call backs off; an empty long poll just polls again.
                await asyncio.sleep(max(0.0, 3 - (time.monotonic() - started)))
                continue
            for update in result:
                self.offset = max(self.offset, update.get('update_id', -1) + 1)
                task = asyncio.ensure_future(self.dispatch(update))
                self.update_tasks.add(task)
                task.add_done_callback(self.update_tasks.discard)

    async def run(self):
        try:
            await asyncio.gather(self.poll_loop(), self.notify_loop())
        except asyncio.CancelledError:
            raise
        except Exception as error:
            logger.exception('Telegram bot stopped unexpectedly (%s)', type(error).__name__)

    async def close(self):
        self.running = False
        tasks = list(self.login_tasks) + list(self.update_tasks)
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        await self.http.aclose()
        if self.poll_http is not self.http:
            await self.poll_http.aclose()
