import json
import time
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from app import protocol
from app.main import create_app
from app.telegram import TelegramBot
from test_telegram import Recorder, callback, message
from test_pass_permission import FakePulse, PASSWORD

KEY = 'a-secure-app-client-key-at-least-32-characters'
OTHER = 'another-app-client-key-at-least-32-characters'


@pytest.fixture
def setup(tmp_path):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(app) as client:
        service = app.state.service
        owner = service.vault.upsert('owner', 'Owner', {'cookie': 'session=owner', 'pass_number': '12345'})['id']
        other = service.vault.upsert('other', 'Other', {'cookie': 'session=other', 'pass_number': '67890'})['id']
        for account in (owner, other):  # new accounts start with the pass off
            service.vault.set_pass_allowed(account, True)
        service.agreements.verify_owner(100, owner)
        service.agreements.verify_owner(200, other)
        bot = TelegramBot('test-token-not-real', service)
        bot.username = 'test_agreement_bot'
        bot.call = Recorder().call
        bot.recorder = bot.call.__self__
        service.telegram = bot
        yield client, service, bot, owner, other


def request(service, account, key=KEY, person=300):
    identifier = service.agreements.create(account, key)
    assert service.agreements.claim(identifier, person, 'Requester · Telegram ID ' + str(person))
    return identifier


def test_permission_is_for_one_person_one_owner_and_cannot_be_forged(setup):
    client, service, _, owner, other = setup
    agreements = service.agreements
    client.headers['x-pass-client'] = KEY
    path = f'/api/accounts/{owner}/rpc/pass-token'
    assert client.get('/api/accounts').json()['passes'][1]['number'] is None
    assert client.post(path, content=protocol.frame(b'')).status_code == 403
    identifier = request(service, owner)
    assert agreements.status(owner, KEY) == 'pending'
    assert not agreements.decide(identifier, 200, True)
    assert not agreements.decide(identifier, 300, True)
    assert agreements.decide(identifier, 100, True)
    assert not agreements.decide(identifier, 100, False)  # replay cannot undo a decision
    assert agreements.status(owner, KEY) == 'approved'
    assert client.post(path, content=protocol.frame(b'')).status_code == 200
    snapshot = client.get('/api/accounts').json()
    rows = {row['account_id']: row['number'] for row in snapshot['passes']}
    assert rows[owner] == '12345' and rows[other] is None
    assert agreements.status(owner, OTHER) != 'approved'
    assert client.post(f'/api/accounts/{other}/rpc/pass-token', content=protocol.frame(b'')).status_code == 403
    assert client.post(path, content=protocol.frame(b''), headers={'x-pass-client': 'forged-key'}).status_code == 403
    grant = agreements.grants(100, owner)[0]
    assert not agreements.revoke(grant['id'], 200)
    assert agreements.revoke(grant['id'], 100)
    assert agreements.status(owner, KEY) == 'denied'
    assert client.post(path, content=protocol.frame(b'')).status_code == 403
    assert client.put(f'/api/accounts/{owner}/pass', json={'number': '888', 'expected_version': 0}).status_code == 403
    assert any(row['kind'] == 'pass_agreement' for row in client.get('/api/events?after=0&wait=0').json()['events'])
    assert KEY not in str([dict(row) for row in service.vault.db.execute('SELECT * FROM audit_events')])


def test_denial_expiry_replay_and_same_person_on_second_phone(setup):
    _, service, _, owner, _ = setup
    agreements = service.agreements
    identifier = request(service, owner)
    assert not agreements.claim(identifier, 999, 'Imposter')
    assert agreements.decide(identifier, 100, False)
    assert agreements.status(owner, KEY) == 'denied'
    expired = agreements.create(owner, OTHER)
    with service.vault.transaction() as db:
        db.execute('UPDATE pass_requests SET expires=? WHERE id=?', (time.time() - 1, expired))
    assert agreements.preview(expired) is None
    assert not agreements.claim(expired, 300, 'Expired')
    approved = request(service, owner)
    assert agreements.decide(approved, 100, True)
    second = request(service, owner, OTHER)
    assert agreements.status(owner, OTHER) == 'approved'
    assert agreements.notifications() == []
    assert not agreements.claim(second, 301, 'Wrong person')
    grant = agreements.grants(100, owner)[0]
    assert agreements.revoke(grant['id'], 100)
    assert agreements.status(owner, KEY) == agreements.status(owner, OTHER) == 'denied'


def test_request_endpoint_keeps_app_credential_out_of_telegram_link(setup):
    client, service, _, owner, _ = setup
    path = f'/api/accounts/{owner}/pass-request'
    assert client.post(path).status_code == 400
    response = client.post(path, headers={'x-pass-client': KEY})
    assert response.status_code == 200
    assert response.json()['url'].startswith('https://t.me/test_agreement_bot?start=access_')
    assert KEY not in response.text
    assert client.post('/api/accounts/missing/pass-request', headers={'x-pass-client': KEY}).status_code == 404
    service.telegram = None
    assert client.post(path, headers={'x-pass-client': KEY}).status_code == 503


@pytest.mark.asyncio
async def test_bot_requires_requester_confirmation_and_verified_owner_callback(setup):
    _, service, bot, owner, _ = setup
    identifier = service.agreements.create(owner, KEY)
    await bot.handle_update({'message': message(300, '/start access_' + identifier)})
    assert service.agreements.status(owner, KEY) != 'approved'
    await bot.handle_update({'callback_query': callback(300, 'request:' + identifier)})
    assert service.agreements.status(owner, KEY) == 'pending'
    assert any('agree:' + identifier in keyboard for keyboard in bot.recorder.keyboards())
    forged = callback(100, 'agree:' + identifier + ':1')
    forged['from']['id'] = 999
    await bot.handle_update({'callback_query': forged})
    assert service.agreements.status(owner, KEY) == 'pending'
    group = callback(100, 'agree:' + identifier + ':1')
    group['message']['chat']['type'] = 'group'
    await bot.handle_update({'callback_query': group})
    assert service.agreements.status(owner, KEY) == 'pending'
    await bot.handle_update({'callback_query': callback(100, 'agree:' + identifier + ':1')})
    assert service.agreements.status(owner, KEY) == 'approved'
    grant = service.agreements.grants(100, owner)[0]
    await bot.handle_update({'callback_query': callback(100, 'revoke:' + grant['id'])})
    assert service.agreements.status(owner, KEY) == 'denied'


@pytest.mark.asyncio
async def test_selecting_account_does_not_prove_ownership_or_allow_global_toggle(setup):
    _, service, bot, owner, _ = setup
    await bot.handle_update({'callback_query': callback(999, 'pick:' + owner)})
    assert not service.agreements.owns(999, owner)
    await bot.handle_update({'callback_query': callback(999, 'pass:' + owner + ':0')})
    assert service.vault.account(owner)['pass_allowed'] == 1
    service.vault.link_telegram(999, owner)  # old alert links are not ownership proof
    assert not service.agreements.owns(999, owner)
    assert service.agreements.grants(999, owner) == []


def test_global_disable_overrides_person_grant_and_testing_is_separate(setup):
    client, service, _, owner, _ = setup
    from app.pass_testing import daily_access
    identifier = request(service, owner)
    assert service.agreements.decide(identifier, 100, True)
    service.vault.set_pass_allowed(owner, False)
    assert service.agreements.status(owner, KEY) == 'disabled'
    path = f'/api/accounts/{owner}/rpc/pass-token'
    assert client.post(path, headers={'x-pass-client': KEY}, content=protocol.frame(b'')).status_code == 403
    password = daily_access(service.vault.pass_testing_key)['password']
    assert client.post(path, headers={'x-pass-test-key': password}, content=protocol.frame(b'')).status_code == 200
    assert service.agreements.status(owner, OTHER) == 'disabled'


def test_grants_survive_restart_and_unlink_revokes_owner_authority(tmp_path):
    from app.agreements import Agreements
    from app.vault import Vault
    vault = Vault(tmp_path, PASSWORD)
    agreements = Agreements(vault)
    owner = vault.upsert('owner', 'Owner', {'cookie': 'session=owner'})['id']
    vault.set_pass_allowed(owner, True)
    agreements.verify_owner(100, owner)
    identifier = agreements.create(owner, KEY)
    assert agreements.claim(identifier, 300, 'Requester · 300')
    assert agreements.decide(identifier, 100, True)
    vault.close()
    reopened = Vault(tmp_path, PASSWORD)
    restored = Agreements(reopened)
    assert restored.status(owner, KEY) == 'approved'
    assert KEY not in str([dict(r) for r in reopened.db.execute('SELECT * FROM pass_clients')])
    restored.unlink_owner(100, owner)
    assert restored.status(owner, KEY) != 'approved'
    assert not restored.revoke(restored.vault.db.execute('SELECT id FROM pass_grants').fetchone()[0], 100)
    reopened.delete_account(owner)
    assert reopened.db.execute('SELECT COUNT(*) FROM pass_grants').fetchone()[0] == 0
    assert reopened.db.execute('SELECT COUNT(*) FROM pass_requests').fetchone()[0] == 0
    reopened.close()


@pytest.mark.asyncio
async def test_login_does_not_block_other_bot_updates(setup):
    import asyncio
    _, _, bot, _, _ = setup
    started, release = asyncio.Event(), asyncio.Event()
    async def slow_login(*args):
        started.set()
        await release.wait()
    bot.start_renewal = slow_login
    await bot.queue_login(100, None, 'Owner')
    await started.wait()
    await bot.handle_update({'message': message(300, '/menu')})
    assert any('keep your MIREA session connected' in text for text in bot.recorder.texts())
    assert len(bot.login_tasks) == 1
    release.set()
    await asyncio.gather(*list(bot.login_tasks))
