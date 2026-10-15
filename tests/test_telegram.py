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


@pytest.mark.asyncio
async def test_start_shows_main_menu(bot):
    instance, _ = bot
    await instance.handle_update({'message': message(100, '/start')})
    assert any('keep your MIREA session connected' in t for t in instance.recorder.texts())
    assert any('callback_data' in k and 'mine' in k for k in instance.recorder.keyboards())


@pytest.mark.asyncio
async def test_pick_requires_university_proof_before_linking_owner(bot):
    instance, service = bot
    row = service.vault.upsert('owner', 'Ivan', {'cookie': 'session=owner'})
    await instance.handle_update({'callback_query': callback(100, 'pick:' + row['id'])})
    assert service.vault.telegram_accounts(100) == []
    assert any('renew:' + row['id'] in k for k in instance.recorder.keyboards())


@pytest.mark.asyncio
async def test_unlink_removes_alert_subscription(bot):
    instance, service = bot
    row = service.vault.upsert('owner', 'Ivan', {'cookie': 'session=owner'})
    service.vault.link_telegram(100, row['id'])
    await instance.handle_update({'callback_query': callback(100, 'unlink:' + row['id'])})
    assert service.vault.telegram_accounts(100) == []


@pytest.mark.asyncio
async def test_add_flow_prompts_then_enrolls(bot):
    instance, service = bot
    created = service.vault.upsert('newbie', 'New', {'cookie': 'session=newbie'})
    await instance.handle_update({'callback_query': callback(100, 'add')})
    assert instance.pending[100] == 'await_label'
    job = SimpleNamespace(id='j', status='complete', qr=None, result_id=created['id'], error=None)
    service.browser.start = lambda label, account_id=None: (job, 'tok')
    await instance.handle_update({'message': message(100, 'Ivan group 23')})
    await asyncio.sleep(0)
    assert 100 not in instance.pending
    assert service.vault.telegram_accounts(100) == [created['id']]
    assert any('Connected' in t for t in instance.recorder.texts('editMessageText'))


@pytest.mark.asyncio
async def test_notify_scan_alerts_only_on_transition(bot):
    instance, service = bot
    row = service.vault.upsert('owner', 'Ivan', {'cookie': 'session=owner'})
    service.vault.link_telegram(100, row['id'])
    await instance.notify_scan()  # seeds state as 'valid', no alert
    assert instance.recorder.texts() == []
    service.vault.update_state(row['id'], 'expired', row['version'])
    await instance.notify_scan()  # valid -> expired: one alert
    alerts = [t for t in instance.recorder.texts() if 'needs a new login' in t]
    assert len(alerts) == 1
    await instance.notify_scan()  # still expired: no repeat
    assert len([t for t in instance.recorder.texts() if 'needs a new login' in t]) == 1


@pytest.mark.asyncio
async def test_renew_missing_account_is_handled(bot):
    instance, _ = bot
    await instance.handle_update({'callback_query': callback(100, 'renew:nope')})
    assert any('no longer exists' in t for t in instance.recorder.texts())


@pytest.mark.asyncio
async def test_https_public_url_offers_mini_app_button(tmp_path):
    vault = Vault(tmp_path, 'a-long-enough-test-password')
    instance = TelegramBot('token', Service(vault, SimpleNamespace()), public_url='https://aa.example.ru')
    instance.call = Recorder().call
    instance.recorder = instance.call.__self__
    await instance.main_menu(100)
    assert any('web_app' in k for k in instance.recorder.keyboards())
    vault.close()
