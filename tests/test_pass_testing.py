from datetime import datetime, timezone

from fastapi.testclient import TestClient

from app import protocol
from app.main import create_app
from app.pass_testing import daily_access, matches
from app.pulse import Reply

PASSWORD = 'test-admin-password-with-enough-characters'
GRPC = {'content-type': 'application/grpc-web+proto'}


class FakePulse:
    async def rpc(self, *args):
        return Reply(200, GRPC, protocol.frame(b'') + protocol.frame(b'grpc-status: 0\r\n', 128))

    async def close(self):
        pass


def test_daily_password_rotates_at_moscow_midnight_and_survives_restart():
    key = b'a-stable-test-key'
    before = datetime(2026, 10, 15, 20, 59, 59, tzinfo=timezone.utc).timestamp()
    midnight = before + 1
    first = daily_access(key, before)
    assert first['day'] == '2026-10-15'
    assert first['expires_at'] == midnight
    assert daily_access(key, before) == first
    assert not matches(first['password'], daily_access(key, midnight))
    assert matches(first['password'].replace('-', '').lower(), first)
    assert not matches('неверный пароль', first)
    assert daily_access(b'another-key', before)['password'] != first['password']


def test_admin_only_code_unlocks_pass_permission_exception_without_changing_agreement(tmp_path):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(app) as client:
        vault = app.state.service.vault
        account = vault.upsert('owner', 'Owner', {'cookie': 'session=owner', 'pass_number': '12345'})
        vault.set_pass_allowed(account['id'], False)
        path = f'/api/accounts/{account["id"]}/rpc/pass-token'
        body = protocol.frame(b'')
        assert client.get('/panel/api/state').status_code == 401
        assert client.get('/api/accounts').json()['passes'][0]['number'] is None
        assert client.post(path, content=body, headers=GRPC).status_code == 403
        assert client.post('/api/debug/pass-testing', json={'password': 'wrong'}).status_code == 401
        client.post('/panel/api/login', json={'password': PASSWORD})
        state = client.get('/panel/api/state').json()
        access = state['pass_testing']
        assert access['timezone'] == 'Europe/Moscow'
        assert client.post('/api/debug/pass-testing', json={'password': access['password']}).json()['expires_at'] == access['expires_at']
        headers = {**GRPC, 'X-Pass-Test-Key': access['password']}
        assert client.post(path, content=body, headers=headers).status_code == 200
        publish = f'/api/accounts/{account["id"]}/pass'
        payload = {'number': '12345', 'expected_version': vault.account(account['id'])['pass_version']}
        assert client.put(publish, json=payload).status_code == 403
        assert client.put(publish, json=payload, headers={'X-Pass-Test-Key': access['password']}).status_code == 200
        snapshot = client.get('/api/accounts', headers=headers).json()
        assert snapshot['passes'][0]['number'] == '12345'
        assert snapshot['accounts'][0]['pass_allowed'] is False
        assert vault.account(account['id'])['pass_allowed'] == 0
        assert client.get('/api/accounts').json()['passes'][0]['number'] is None
        assert client.post(path, content=body, headers=GRPC).status_code == 403
        for method in ('pass-code', 'pass-issue'):
            assert client.post(f'/api/accounts/{account["id"]}/rpc/{method}', content=body, headers=headers).status_code == 403
        events = [dict(row) for row in vault.db.execute('SELECT * FROM audit_events')]
        assert {event['action'] for event in events} >= {'pass_testing_unlocked', 'pass_testing_pass-token'}
        assert access['password'] not in str(events)
        saved_code = access['password']
    reopened = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(reopened) as client:
        client.post('/panel/api/login', json={'password': PASSWORD})
        assert client.get('/panel/api/state').json()['pass_testing']['password'] == saved_code


def test_wrong_password_rate_limited_and_old_day_rejected(tmp_path, monkeypatch):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    now = datetime(2026, 10, 15, 20, 59, 59, tzinfo=timezone.utc).timestamp()
    monkeypatch.setattr('app.pass_testing.time.time', lambda: now)
    with TestClient(app) as client:
        client.post('/panel/api/login', json={'password': PASSWORD})
        old = client.get('/panel/api/state').json()['pass_testing']['password']
        now += 1
        assert client.post('/api/debug/pass-testing', json={'password': old}).status_code == 401
        for _ in range(7):
            assert client.post('/api/debug/pass-testing', json={'password': 'wrong'}).status_code == 401
        assert client.post('/api/debug/pass-testing', json={'password': 'wrong'}).status_code == 429
