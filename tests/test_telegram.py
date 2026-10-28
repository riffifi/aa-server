import json
import asyncio
from types import SimpleNamespace
import pytest
from app.main import Service
from app.vault import Vault
from app.telegram import TelegramBot


class Recorder:
    def __init__(self):
        self.calls = []
        self.mid = 0
    async def call(self, method, data=None, files=None, timeout=None):
        self.calls.append((method, data or {}, files))
        if method == 'sendMessage':
            self.mid += 1
            return {'message_id': self.mid}
        return {}

    def texts(self, method='sendMessage'):
        return [data.get('text', '') for name, data, _ in self.calls if name == method]

    def keyboards(self, method='sendMessage'):
        boards = []
        for name, data, _ in self.calls:
            if name == method and 'reply_markup' in data:
                boards.append(json.dumps(data['reply_markup']))
        return boards


@pytest.fixture
def bot(tmp_path):
    vault = Vault(tmp_path, 'a-long-enough-test-password')
    service = Service(vault, SimpleNamespace())
    instance = TelegramBot('token', service, public_url='')
    instance.call = Recorder().call
    instance.recorder = instance.call.__self__
    yield instance, service
    vault.close()


def callback(chat_id, data):
    return {'id': 'cb', 'from': {'id': chat_id}, 'message': {'chat': {'id': chat_id, 'type': 'private'}}, 'data': data}


def message(chat_id, text):
    return {'chat': {'id': chat_id, 'type': 'private'}, 'text': text}


def linked(instance, service, chat_id=100, uid='owner'):
    row = service.vault.upsert(uid, 'Ivan', {'cookie': 'session=' + uid}, group_id=7, group_name='ИКБО-01-24')
    code = service.vault.telegram_link_code('aaid-' + uid, row['id'])
    return row, code


@pytest.mark.asyncio
async def test_start_offers_aa_id_sign_in(tmp_path):
    vault = Vault(tmp_path, 'a-long-enough-test-password')
    instance = TelegramBot('token', Service(vault, SimpleNamespace()), public_url='https://aa.example.ru')
    instance.call = Recorder().call
    instance.recorder = instance.call.__self__
    await instance.handle_update({'message': message(100, '/start')})
    assert any('Войдите через AA ID' in t for t in instance.recorder.texts())
    assert any('https://aa.example.ru/tg/connect' in k for k in instance.recorder.keyboards())
    await instance.close()
    vault.close()


@pytest.mark.asyncio
async def test_aa_id_link_binds_chat_shows_own_account_and_proves_ownership(bot):
    instance, service = bot
    row, code = linked(instance, service)
    other = service.vault.upsert('stranger', 'Stranger', {'cookie': 'session=stranger'})
    await instance.handle_update({'message': message(100, '/start aaid_' + code)})
    user = service.vault.telegram_user(100)
    assert user['account_id'] == row['id'] and user['aaid_id'] == 'aaid-owner'
    assert service.agreements.owns(100, row['id']) and service.vault.telegram_accounts(100) == [row['id']]
    texts = instance.recorder.texts()
    assert any('Telegram привязан' in t for t in texts) and any('Ivan · ИКБО-01-24' in t for t in texts)
    assert not any('Stranger' in t for t in texts)
    # The code works once, and nobody else's chat can use it.
    await instance.handle_update({'message': message(200, '/start aaid_' + code)})
    assert service.vault.telegram_user(200)['account_id'] is None
    assert any('устарела' in t for t in instance.recorder.texts())
    await instance.handle_update({'callback_query': callback(200, 'pick:' + other['id'])})
    assert not service.agreements.owns(200, other['id'])


@pytest.mark.asyncio
async def test_pass_switch_is_confirmed_and_only_for_the_owner(bot):
    instance, service = bot
    row, code = linked(instance, service)
    await instance.handle_update({'message': message(100, '/start aaid_' + code)})
    await instance.handle_update({'callback_query': callback(100, f'pass:{row["id"]}:1')})
    assert service.vault.account(row['id'])['pass_allowed'] == 0
    assert any(f'passok:{row["id"]}:1' in k for k in instance.recorder.keyboards('editMessageText') + instance.recorder.keyboards())
    await instance.handle_update({'callback_query': callback(300, f'passok:{row["id"]}:1')})
    assert service.vault.account(row['id'])['pass_allowed'] == 0
    await instance.handle_update({'callback_query': callback(100, f'passok:{row["id"]}:1')})
    assert service.vault.account(row['id'])['pass_allowed'] == 1


@pytest.mark.asyncio
async def test_language_setting_switches_every_text(bot):
    instance, service = bot
    row, code = linked(instance, service)
    await instance.handle_update({'message': message(100, '/start aaid_' + code)})
    await instance.handle_update({'callback_query': callback(100, 'lang:en')})
    assert service.vault.telegram_user(100)['lang'] == 'en'
    assert any('Language: English' in t for t in instance.recorder.texts('editMessageText') + instance.recorder.texts())
    await instance.handle_update({'message': message(100, '/menu')})
    assert 'MIREA session: ✅ working' in instance.recorder.texts()[-1]
    await instance.handle_update({'callback_query': callback(100, 'lang:xx')})
    assert service.vault.telegram_user(100)['lang'] == 'en'
    assert service.vault.telegram_user(555)['lang'] == 'ru'   # Russian by default


@pytest.mark.asyncio
async def test_unlink_aa_id_needs_confirmation(bot):
    instance, service = bot
    row, code = linked(instance, service)
    await instance.handle_update({'message': message(100, '/start aaid_' + code)})
    await instance.handle_update({'callback_query': callback(100, 'logout')})
    assert service.vault.telegram_user(100)['account_id'] == row['id']
    await instance.handle_update({'callback_query': callback(100, 'logoutok')})
    assert service.vault.telegram_user(100)['account_id'] is None
    assert not service.agreements.owns(100, row['id'])


@pytest.mark.asyncio
async def test_renew_is_only_for_the_owner_and_reports_success(bot):
    instance, service = bot
    row, code = linked(instance, service)
    job = SimpleNamespace(id='j', status='complete', qr=None, result_id=row['id'], error=None)
    service.browser.start = lambda label, account_id=None: (job, 'tok')
    await instance.handle_update({'callback_query': callback(300, 'renew:' + row['id'])})
    assert not instance.login_tasks
    await instance.handle_update({'message': message(100, '/start aaid_' + code)})
    await instance.handle_update({'callback_query': callback(100, 'renew:' + row['id'])})
    await asyncio.gather(*instance.login_tasks)
    assert any('Готово' in t for t in instance.recorder.texts('editMessageText'))


@pytest.mark.asyncio
async def test_notify_scan_alerts_only_on_transition_and_respects_the_setting(bot):
    instance, service = bot
    row = service.vault.upsert('owner', 'Ivan', {'cookie': 'session=owner'})
    service.vault.link_telegram(100, row['id'])
    await instance.notify_scan()  # seeds state as 'valid', no alert
    assert instance.recorder.texts() == []
    service.vault.update_state(row['id'], 'expired', row['version'])
    await instance.notify_scan()  # valid -> expired: one alert
    alerts = [t for t in instance.recorder.texts() if 'нового входа' in t]
    assert len(alerts) == 1
    await instance.notify_scan()  # still expired: no repeat
    assert len([t for t in instance.recorder.texts() if 'нового входа' in t]) == 1
    service.vault.update_state(row['id'], 'valid', service.vault.account(row['id'])['version'])
    await instance.notify_scan()
    service.vault.update_telegram_user(100, alerts=0)
    service.vault.update_state(row['id'], 'expired', service.vault.account(row['id'])['version'])
    await instance.notify_scan()
    assert len([t for t in instance.recorder.texts() if 'нового входа' in t]) == 1


@pytest.mark.asyncio
async def test_renew_missing_account_is_handled(bot):
    instance, _ = bot
    await instance.handle_update({'callback_query': callback(100, 'renew:nope')})
    assert any('не найден' in t for t in instance.recorder.texts())


@pytest.mark.asyncio
async def test_slow_chat_does_not_hold_other_chats(bot):
    instance, _ = bot
    release = asyncio.Event()
    seen = []
    async def handle(update):
        chat = update['message']['chat']['id']
        if chat == 1:
            await release.wait()
        seen.append(chat)
    instance.handle_update = handle
    slow = asyncio.ensure_future(instance.dispatch({'message': message(1, '/menu')}))
    await instance.dispatch({'message': message(2, '/menu')})
    assert seen == [2]
    release.set()
    await slow
    assert seen == [2, 1]


@pytest.mark.asyncio
async def test_https_public_url_offers_mini_app_button(tmp_path):
    vault = Vault(tmp_path, 'a-long-enough-test-password')
    instance = TelegramBot('token', Service(vault, SimpleNamespace()), public_url='https://aa.example.ru')
    instance.call = Recorder().call
    instance.recorder = instance.call.__self__
    row, code = linked(instance, instance.service)
    await instance.link_aaid(100, code)
    assert any('web_app' in k and '/today' in k for k in instance.recorder.keyboards())
    await instance.close()
    vault.close()


@pytest.mark.asyncio
async def test_authenticated_relay_keeps_bot_token_out_of_request_url(bot):
    import httpx
    _, service = bot
    async def handle(request):
        assert str(request.url) == 'https://relay.example/getMe'
        assert request.headers['authorization'] == 'Bearer a-private-relay-key-at-least-32-characters'
        assert 'secret-test-token' not in str(request.url)
        return httpx.Response(200, json={'ok': True, 'result': {'username': 'test_bot'}})
    http = httpx.AsyncClient(transport=httpx.MockTransport(handle))
    instance = TelegramBot('secret-test-token', service, http=http,
                           relay_url='https://relay.example', relay_key='a-private-relay-key-at-least-32-characters')
    assert await instance.call('getMe') == {'username': 'test_bot'}
    await instance.close()


@pytest.mark.parametrize('url,key', [('http://relay.example', 'x' * 32), ('https://user:password@relay.example', 'x' * 32), ('https://relay.example?token=secret', 'x' * 32), ('https://relay.example', ''), ('', 'x' * 32)])
def test_invalid_relay_configuration_is_rejected(bot, url, key):
    _, service = bot
    with pytest.raises(ValueError, match='TELEGRAM_RELAY_URL'):
        TelegramBot('test-token', service, relay_url=url, relay_key=key)
