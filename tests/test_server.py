import base64
import json
import time
import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app.pulse import Reply, validate_header
from app.vault import Conflict, Vault
from app import protocol

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
        return self.state, cookie.split('=', 1)[1] if self.state == 'valid' else None
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
        yield client, app.state.service, pulse, tmp_path

def key(service, scopes=None, publisher=False):
    identifier, value = service.vault.create_device('Phone', scopes or ['*'], publisher)
    return identifier, {'Authorization': 'Bearer ' + value}

def account(service, uid='person-one'):
    return service.vault.upsert(uid, 'Student', {'cookie': 'session=' + uid})

def test_panel_auth_csrf_and_revocation(setup):
    client, service, _, _ = setup
    assert client.get('/health').json()['ok']
    assert client.get('/panel/api/state').status_code == 401
    assert client.post('/panel/api/login', json={'password': PASSWORD}, headers={'Origin': 'http://other.host'}).status_code == 403
    login = client.post('/panel/api/login', json={'password': PASSWORD})
    assert login.status_code == 200
    assert 'httponly' in login.headers['set-cookie'].lower()
    assert client.post('/panel/api/devices', json={'name': 'Phone'}).status_code == 403
    csrf = {'X-CSRF-Token': login.json()['csrf']}
    device = client.post('/panel/api/devices', json={'name': 'Phone'}, headers=csrf).json()
    auth = {'Authorization': 'Bearer ' + device['key']}
    assert client.get('/api/accounts', headers=auth).status_code == 200
    assert client.delete('/panel/api/devices/' + device['id'], headers=csrf).status_code == 200
    assert client.get('/api/accounts', headers=auth).status_code == 401
    assert client.post('/panel/api/logout', headers=csrf).status_code == 200
    assert client.get('/panel/api/state').status_code == 401

def test_accounts_are_encrypted_and_scope_filtered(setup):
    client, service, _, directory = setup
    first, second = account(service), account(service, 'person-two')
    service.vault.save_pass(first['id'], '123456789')
    _, auth = key(service, [first['id']])
    result = client.get('/api/accounts', headers=auth).json()
    assert [row['id'] for row in result['accounts']] == [first['id']]
    assert result['passes'][0]['number'] == '123456789'
    assert 'cookie' not in json.dumps(result) and 'secret' not in json.dumps(result)
    events = client.get('/api/events?after=0&wait=0', headers=auth).json()
    assert all(row['account_id'] == first['id'] for row in events['events'])
    assert client.post('/api/accounts/' + second['id'] + '/rpc/profile', headers=auth, content=protocol.profile_request()).status_code == 403
    encrypted = service.vault.account(first['id'])['secret']
    assert '123456789' not in encrypted and 'session=person-one' not in encrypted
    assert (directory / 'master.key').stat().st_mode & 0o777 == 0o600

def test_publish_versions_and_identity_binding(setup):
    client, service, pulse, _ = setup
    _, receiver = key(service)
    _, auth = key(service, publisher=True)
    body = {'label': 'Owner', 'cookie': 'session=owner'}
    assert client.post('/api/accounts/import', headers=receiver, json=body).status_code == 403
    created = client.post('/api/accounts/import', headers=auth, json=body)
    assert created.status_code == 200
    row = created.json()
    assert 'secret' not in row
    # Unlinked copies adopt the existing authoritative version.
    assert client.post('/api/accounts/import', headers=auth, json=body).json()['version'] == row['version']
    body.update(account_id=row['id'], expected_version=row['version'])
    updated = client.post('/api/accounts/import', headers=auth, json=body)
    assert updated.status_code == 200
    assert client.post('/api/accounts/import', headers=auth, json=body).status_code == 409
    body.update(cookie='session=someone-else', expected_version=updated.json()['version'])
    assert client.post('/api/accounts/import', headers=auth, json=body).status_code == 409
    pulse.state = 'expired'
    assert client.post('/api/accounts/import', headers=auth, json=body).status_code == 422

def test_pass_sync_cas_and_events(setup):
    client, service, _, _ = setup
    row = account(service)
    _, auth = key(service, publisher=True)
    _, receiver = key(service)
    url = '/api/accounts/' + row['id'] + '/pass'
    body = {'number': '1234', 'expected_version': 0}
    assert client.put(url, headers=receiver, json=body).status_code == 403
    cursor = service.vault.cursor()
    assert client.put(url, headers=auth, json=body).status_code == 200
    assert client.put(url, headers=auth, json=body).status_code == 409
    assert client.put(url, headers=auth, json={'number': '9223372036854775808', 'expected_version': 1}).status_code == 400
    events = client.get(f'/api/events?after={cursor}&wait=0', headers=receiver).json()
    assert events['events'][0]['kind'] == 'pass'
    assert client.get('/api/accounts', headers=receiver).json()['passes'][0]['number'] == '1234'
    current = service.vault.account(row['id'])
    renewed = service.vault.upsert('person-one', 'Student', {'cookie': 'session=new-cookie'}, expected=current['version'])
    assert service.vault.secret(renewed)['pass_number'] == '1234'
    service.vault.update_state(row['id'], 'expired', row['version'])
    assert service.vault.account(row['id'])['state'] == 'valid'

def test_rpc_allowlist_tokens_are_account_bound_and_pass_is_captured(setup):
    client, service, pulse, _ = setup
    row, other = account(service), account(service, 'other')
    _, auth = key(service, publisher=True)
    _, reader = key(service)
    base = '/api/accounts/' + row['id'] + '/rpc/'
    frame = protocol.frame(b'')
    assert client.post(base + 'arbitrary', headers=auth, content=frame).status_code == 404
    assert client.post(base + 'profile', headers=auth, content=b'bad').status_code == 400
    assert client.post(base + 'pass-code', headers=reader, content=frame).status_code == 403
    assert client.post(base + 'pass-code', headers=auth, content=frame).status_code == 403
    payload = base64.urlsafe_b64encode(json.dumps({'exp': int(time.time()) + 300}).encode()).decode().rstrip('=')
    token = 'header.' + payload + '.signature'
    pulse.response = reply(protocol.field(1, token.encode()))
    assert client.post(base + 'pass-token', headers=reader, content=frame).status_code == 200
    token_auth = {**auth, 'X-Pulse-Token': token}
    assert client.post('/api/accounts/' + other['id'] + '/rpc/pass-code', headers=token_auth, content=frame).status_code == 403
    pulse.response = reply(protocol.field(1, b'\x08' + protocol.varint(4321)))
    assert client.post(base + 'pass-issue', headers=token_auth, content=frame).status_code == 200
    assert service.vault.secret(service.vault.account(row['id']))['pass_number'] == '4321'
    assert pulse.calls[-1][0] == 'session=person-one'
    assert pulse.calls[-1][3] == token
    pulse.response = Reply(401, {}, b'')
    upstream = client.post(base + 'profile', headers=auth, content=frame)
    assert upstream.status_code == 200
    assert protocol.grpc_message(upstream.content, upstream.headers)[1] == 16

def test_limits_including_chunked_and_cookie_validation(setup):
    client, service, _, _ = setup
    _, auth = key(service, publisher=True)
    assert client.post('/api/accounts/import', headers=auth, content=b'x' * 200001).status_code == 413
    assert client.post('/api/accounts/import', headers=auth, content=iter([b'x' * 100001, b'y' * 100001])).status_code == 413
    for value in ['cookie=one\r\nInjected: value', 'a=b;a=c', 'bad cookie=x']:
        with pytest.raises(ValueError):
            validate_header(value)
    assert client.get('/api/events?after=-1&wait=0', headers=auth).status_code == 400
    assert client.get('/api/events?wait=26', headers=auth).status_code == 400

def test_profile_protocol_never_accepts_html_or_gateway_error():
    identity = protocol.field(1, protocol.field(1, protocol.field(1, b'uid')))
    value = reply(identity)
    assert protocol.authentication_state(value.status, value.headers, value.body) == ('valid', 'uid')
    assert protocol.authentication_state(403, {}, b'blocked')[0] == 'forbidden'
    assert protocol.authentication_state(200, {'content-type': 'text/html'}, b'<html/>')[0] == 'invalidResponse'
    assert protocol.authentication_state(302, {'location': 'https://sso.mirea.ru/realms/mirea'}, b'')[0] == 'expired'
    assert protocol.authentication_state(503, {}, b'')[0] == 'unavailable'
    for body in [b'\x00', protocol.frame(b'', 1), protocol.frame(b'')]:
        with pytest.raises(ValueError):
            protocol.grpc_message(body, HEADERS)

def test_data_survives_restart_and_bootstrap_password_is_preserved(tmp_path):
    vault = Vault(tmp_path, PASSWORD)
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=encrypted-owner'})
    vault.save_pass(row['id'], '4567')
    _, token = vault.create_device('Phone', ['*'])
    vault.close()
    reopened = Vault(tmp_path, 'different-bootstrap-password')
    assert reopened.admin_login(PASSWORD)
    assert not reopened.admin_login('different-bootstrap-password')
    assert reopened.device(token)
    assert reopened.secret(reopened.account(row['id']))['pass_number'] == '4567'
    reopened.close()
