import base64
import json
import time
import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app.pulse import Reply, validate_header
from app.vault import Vault
from app import protocol
from app.pass_testing import daily_access

PASSWORD = 'a-test-password-with-enough-characters'
HEADERS = {'content-type': 'application/grpc-web+proto'}

def reply(message=b'', status=0):
    return Reply(200, HEADERS, protocol.frame(message) + protocol.frame(f'grpc-status: {status}\r\n'.encode(), 128))

class FakePulse:
    def __init__(self):
        self.state = 'valid'
        self.calls = []
        self.response = reply()
    async def identity(self, cookie):
        return self.state, cookie.split('=', 1)[1].split(':')[0] if self.state == 'valid' else None
    async def rpc(self, cookie, method, body, authorization=None):
        self.calls.append((cookie, method, body, authorization))
        return self.response
    async def close(self):
        pass

@pytest.fixture
def setup(tmp_path):
    pulse = FakePulse()
    app = create_app(tmp_path, PASSWORD, pulse, background=False)
    with TestClient(app) as client:
        client.headers['x-pass-test-key'] = daily_access(app.state.service.vault.pass_testing_key)['password']
        yield client, app.state.service, pulse, tmp_path

def account(service, uid='person-one'):
    return service.vault.upsert(uid, 'Student', {'cookie': 'session=' + uid})

def test_url_only_api_and_panel_password_csrf(setup):
    client, service, _, _ = setup
    assert client.get('/health').json()['protocol'] == 2
    assert client.get('/api/accounts').status_code == 200
    assert client.get('/api/accounts', headers={'Authorization': 'Bearer ignored-old-key'}).status_code == 200
    assert client.get('/panel/api/state').status_code == 401
    assert client.post('/panel/api/login', json={'password': PASSWORD}, headers={'Origin': 'http://other.host'}).status_code == 403
    login = client.post('/panel/api/login', json={'password': PASSWORD})
    assert login.status_code == 200
    assert 'httponly' in login.headers['set-cookie'].lower()
    row = account(service)
    assert client.delete('/panel/api/accounts/' + row['id']).status_code == 403
    csrf = {'X-CSRF-Token': login.json()['csrf']}
    state = client.get('/panel/api/state').json()
    assert 'devices' not in state
    assert client.post('/panel/api/devices', headers=csrf, json={'name': 'Phone'}).status_code == 404
    assert client.post('/panel/api/logout', headers=csrf).status_code == 200
    assert client.get('/panel/api/state').status_code == 401

def test_accounts_are_encrypted_and_shared_without_cookies(setup):
    client, service, _, directory = setup
    first, second = account(service), account(service, 'person-two')
    service.vault.save_pass(first['id'], '123456789')
    result = client.get('/api/accounts').json()
    assert {row['id'] for row in result['accounts']} == {first['id'], second['id']}
    assert result['passes'][0]['number'] == '123456789'
    assert result['sync'] == 'automatic'
    assert 'cookie' not in json.dumps(result) and 'secret' not in json.dumps(result)
    events = client.get('/api/events?after=0&wait=0').json()
    assert {row['account_id'] for row in events['events']} == {first['id'], second['id']}
    encrypted = service.vault.account(first['id'])['secret']
    assert '123456789' not in encrypted and 'session=person-one' not in encrypted
    assert (directory / 'master.key').stat().st_mode & 0o777 == 0o600

def test_publish_versions_and_identity_binding(setup):
    client, service, pulse, _ = setup
    body = {'label': 'Owner', 'cookie': 'session=owner:initial'}
    created = client.post('/api/accounts/import', json=body)
    assert created.status_code == 200
    row = created.json()
    assert 'secret' not in row
    assert client.post('/api/accounts/import', json=body).json()['version'] == row['version']
    body.update(account_id=row['id'], expected_session_version=row['session_version'], cookie='session=owner:renewed')
    updated = client.post('/api/accounts/import', json=body)
    assert updated.status_code == 200
    assert updated.json()['session_version'] == row['session_version'] + 1
    assert client.post('/api/accounts/import', json=body).status_code == 409
    body.update(cookie='session=someone-else', expected_session_version=updated.json()['session_version'])
    assert client.post('/api/accounts/import', json=body).status_code == 409
    pulse.state = 'expired'
    assert client.post('/api/accounts/import', json=body).status_code == 422

def test_pass_sync_cas_events_and_session_revisions_are_independent(setup):
    client, service, _, _ = setup
    row = account(service)
    url = '/api/accounts/' + row['id'] + '/pass'
    body = {'number': '1234', 'expected_version': 0}
    cursor = service.vault.cursor()
    assert client.put(url, json=body).status_code == 200
    assert client.put(url, json=body).status_code == 409
    assert client.put(url, json={'number': '9223372036854775808', 'expected_version': 1}).status_code == 400
    assert client.get(f'/api/events?after={cursor}&wait=0').json()['events'][0]['kind'] == 'pass'
    assert client.get('/api/accounts').json()['passes'][0]['number'] == '1234'
    renewed = service.vault.upsert('person-one', 'Student', {'cookie': 'session=new-cookie'}, session_expected=row['session_version'])
    assert service.vault.secret(renewed)['pass_number'] == '1234'
    assert renewed['session_version'] == row['session_version'] + 1
    service.vault.update_state(row['id'], 'expired', row['version'])
    assert service.vault.account(row['id'])['state'] == 'valid'

def test_rpc_allowlist_tokens_are_account_bound_and_pass_is_captured(setup):
    client, service, pulse, _ = setup
    row, other = account(service), account(service, 'other')
    base = '/api/accounts/' + row['id'] + '/rpc/'
    frame = protocol.frame(b'')
    assert client.post(base + 'arbitrary', content=frame).status_code == 404
    assert client.post(base + 'profile', content=b'bad').status_code == 400
    assert client.post(base + 'pass-code', content=frame).status_code == 403
    payload = base64.urlsafe_b64encode(json.dumps({'exp': int(time.time()) + 300}).encode()).decode().rstrip('=')
    token = 'header.' + payload + '.signature'
    pulse.response = reply(protocol.field(1, token.encode()))
    assert client.post(base + 'pass-token', content=frame).status_code == 200
    auth = {'X-Pulse-Token': token}
    assert client.post('/api/accounts/' + other['id'] + '/rpc/pass-code', headers=auth, content=frame).status_code == 403
    pulse.response = reply(protocol.field(1, b'\x08' + protocol.varint(4321)))
    assert client.post(base + 'pass-issue', headers=auth, content=frame).status_code == 200
    assert service.vault.secret(service.vault.account(row['id']))['pass_number'] == '4321'
    assert pulse.calls[-1][0] == 'session=person-one'
    assert pulse.calls[-1][3] == token
    pulse.response = Reply(401, {}, b'')
    upstream = client.post(base + 'profile', content=frame)
    assert upstream.status_code == 200
    assert protocol.grpc_message(upstream.content, upstream.headers)[1] == 16

def test_limits_chunked_cookie_validation_and_cross_origin_writes(setup):
    client, service, _, _ = setup
    assert client.post('/api/accounts/import', content=b'x' * 200001).status_code == 413
    assert client.post('/api/accounts/import', content=iter([b'x' * 100001, b'y' * 100001])).status_code == 413
    for value in ['cookie=one\r\nInjected: value', 'a=b;a=c', 'bad cookie=x']:
        with pytest.raises(ValueError): validate_header(value)
    assert client.get('/api/events?after=-1&wait=0').status_code == 400
    assert client.get('/api/events?wait=26').status_code == 400
    assert client.post('/api/accounts/import', headers={'Origin': 'https://other.host'}, json={'cookie': 'session=owner', 'label': 'Owner'}).status_code == 403

def test_profile_protocol_never_accepts_html_or_gateway_error():
    identity = protocol.field(1, protocol.field(1, protocol.field(1, b'uid')))
    value = reply(identity)
    assert protocol.authentication_state(value.status, value.headers, value.body) == ('valid', 'uid')
    assert protocol.authentication_state(403, {}, b'blocked')[0] == 'forbidden'
    assert protocol.authentication_state(200, {'content-type': 'text/html'}, b'<html/>')[0] == 'invalidResponse'
    assert protocol.authentication_state(302, {'location': 'https://sso.mirea.ru/realms/mirea'}, b'')[0] == 'expired'
    assert protocol.authentication_state(503, {}, b'')[0] == 'unavailable'
    for body in [b'\x00', protocol.frame(b'', 1), protocol.frame(b'')]:
        with pytest.raises(ValueError): protocol.grpc_message(body, HEADERS)

def test_data_survives_restart_and_bootstrap_password_is_preserved(tmp_path):
    vault = Vault(tmp_path, PASSWORD)
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=encrypted-owner'})
    vault.save_pass(row['id'], '4567')
    vault.close()
    reopened = Vault(tmp_path, 'different-bootstrap-password')
    assert reopened.admin_login(PASSWORD)
    assert not reopened.admin_login('different-bootstrap-password')
    assert reopened.secret(reopened.account(row['id']))['pass_number'] == '4567'
    assert reopened.account(row['id'])['session_version'] == 1
    reopened.close()


def test_existing_database_migrates_without_losing_credentials(tmp_path):
    import sqlite3
    vault = Vault(tmp_path, PASSWORD)
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=keep-this-cookie'})
    vault.save_pass(row['id'], '777')
    vault.close()
    db = sqlite3.connect(tmp_path / 'sessions.sqlite3')
    db.execute('ALTER TABLE accounts DROP COLUMN session_version')
    db.commit()
    db.close()
    migrated = Vault(tmp_path, PASSWORD)
    stored = migrated.account(row['id'])
    assert stored['session_version'] == 1
    assert migrated.secret(stored) == {'cookie': 'session=keep-this-cookie', 'pass_number': '777'}
    migrated.close()


def test_late_pass_issuance_response_cannot_overwrite_a_newer_pass(setup):
    from app.vault import digest
    client, service, pulse, _ = setup
    row = account(service)
    service.issued_tokens[row['id']][digest('issued-token')] = time.time() + 300
    async def delayed_reply(cookie, method, body, authorization=None):
        service.vault.save_pass(row['id'], '9999')
        return reply(protocol.field(1, b'\x08' + protocol.varint(1111)))
    pulse.rpc = delayed_reply
    response = client.post('/api/accounts/' + row['id'] + '/rpc/pass-issue',
        headers={'X-Pulse-Token': 'issued-token'}, content=protocol.frame(b''))
    assert response.status_code == 200
    assert service.vault.secret(service.vault.account(row['id']))['pass_number'] == '9999'


def test_group_filtered_accounts_passes_and_group_discovery(setup):
    client, service, _, _ = setup
    first = service.vault.upsert('a', 'Alice', {'cookie': 'session=a'}, group_id=42, group_name='Group A')
    second = service.vault.upsert('b', 'Bob', {'cookie': 'session=b'}, group_id=43, group_name='Group B')
    service.vault.save_pass(first['id'], '111')
    service.vault.save_pass(second['id'], '222')
    result = client.get('/api/accounts?group_id=42').json()
    assert result['group_filter'] == 42
    assert [a['id'] for a in result['accounts']] == [first['id']]
    assert [p['number'] for p in result['passes']] == ['111']
    assert {g['id'] for g in client.get('/api/groups').json()['groups']} == {42, 43}
    assert client.get('/api/accounts?group_id=0').status_code == 400
    assert client.get('/api/events?group_id=-1').status_code == 400


def test_group_move_notifies_both_groups_and_keeps_session_pass(setup):
    client, service, _, _ = setup
    first = service.vault.upsert('a', 'Alice', {'cookie': 'session=a'}, group_id=42, group_name='Group A')
    service.vault.save_pass(first['id'], '111')
    before = service.vault.account(first['id'])
    cursor = service.vault.cursor()
    moved = client.put(f"/api/accounts/{first['id']}/group", json={'group_id': 43, 'group_name': 'Group B', 'expected_version': before['version']})
    assert moved.status_code == 200
    assert moved.json()['session_version'] == before['session_version']
    assert moved.json()['pass_version'] == before['pass_version']
    assert service.vault.secret(service.vault.account(first['id']))['pass_number'] == '111'
    for group in (42, 43):
        events = client.get(f'/api/events?group_id={group}&after={cursor}&wait=0').json()['events']
        assert [e['kind'] for e in events] == ['group']
    assert client.get('/api/accounts?group_id=42').json()['accounts'] == []
    assert len(client.get('/api/accounts?group_id=43').json()['accounts']) == 1
    cursor = service.vault.cursor()
    service.vault.delete_account(first['id'])
    assert client.get(f'/api/events?group_id=43&after={cursor}&wait=0').json()['events'][0]['kind'] == 'deleted'


def test_group_updates_validate_cas_clear_and_panel_csrf(setup):
    client, service, _, _ = setup
    first = account(service)
    path = f"/api/accounts/{first['id']}/group"
    body = {'group_id': 42, 'group_name': 'Group A', 'expected_version': first['version']}
    assert client.put(path, json={**body, 'group_id': -1}).status_code == 422
    assert client.put(path, json=body).status_code == 200
    assert client.put(path, json=body).status_code == 409
    login = client.post('/panel/api/login', json={'password': PASSWORD}).json()
    panel_path = f"/panel/api/accounts/{first['id']}/group"
    cleared = {'group_id': None, 'group_name': None, 'expected_version': first['version'] + 1}
    assert client.put(panel_path, json=cleared).status_code == 403
    assert client.put(panel_path, json=cleared, headers={'X-CSRF-Token': login['csrf']}).json()['group_id'] is None


def test_import_can_assign_an_initial_group_without_replacing_authoritative_session(setup):
    client, service, _, _ = setup
    first = account(service, 'person')
    result = client.post('/api/accounts/import', json={'cookie': 'session=person:older', 'label': 'Alice', 'group_id': 42, 'group_name': 'Group A'})
    assert result.status_code == 200
    assert result.json()['group_id'] == 42
    assert result.json()['session_version'] == first['session_version']
    assert service.vault.secret(service.vault.account(first['id']))['cookie'] == 'session=person'
    result = client.post('/api/accounts/import', json={'cookie': 'session=person:other', 'label': 'Alice', 'group_id': 43, 'group_name': 'Group B'})
    assert result.json()['group_id'] == 42


def test_cookie_renewal_does_not_restore_a_stale_phone_group(setup):
    client, service, _, _ = setup
    first = service.vault.upsert('person', 'Alice', {'cookie': 'session=person'}, group_id=43, group_name='Group B')
    result = client.post('/api/accounts/import', json={'cookie': 'session=person:new', 'label': 'Alice', 'expected_session_version': first['session_version'], 'group_id': 42, 'group_name': 'Old group'})
    assert result.status_code == 200
    assert result.json()['group_id'] == 43
    assert result.json()['group_name'] == 'Group B'
    assert result.json()['session_version'] == first['session_version'] + 1

def site_session(vault, account_id):
    # The account's person signed in to the site with AA ID.
    row = vault.account(account_id)
    return vault.create_site_session('aaid-' + row['pulse_id'], row['pulse_id'], None, None)
