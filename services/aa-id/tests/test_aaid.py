import struct
import time
import pytest
from fastapi.testclient import TestClient
from aaid.main import create_app
from aaid.mirea_login import LoginJob
from aaid import pulse
from aaid.tokens import verify, unb64

class FakePulse:
    def __init__(self):
        self.people = {}
        self.cookies = []
    async def whoami(self, cookie):
        self.cookies.append(cookie)
        pulse.validate_cookie(cookie)
        person = self.people.get(cookie)
        return person if person else (None, None, None)
    async def close(self):
        pass

class FakeLogins:
    def __init__(self):
        self.jobs = {}
    def start(self, digest):
        job = LoginJob('job-%d' % len(self.jobs), digest('t' * 30 + str(len(self.jobs))), time.time(), status='login', qr='https://sso.mirea.ru/realms/mirea/qr-code-auth/scan?x=1')
        self.jobs[job.id] = job
        return job, 't' * 30 + str(len(self.jobs) - 1)
    def approve(self, job_id, identity):
        job = self.jobs[job_id]
        job.status, job.qr, job.identity = 'complete', None, identity
    def qr_png(self, job):
        return b'png' if job.qr else None
    async def close(self):
        pass

@pytest.fixture
def setup(tmp_path, monkeypatch):
    monkeypatch.setenv('MAX_SESSIONS', '2')
    monkeypatch.setenv('ADMIN_TOKEN', 'a' * 32)
    fake = FakePulse()
    fake.people['s=alice'] = ('uid-alice', 4242, 'ИКБО-01-24')
    fake.people['s=alice2'] = ('uid-alice', None, None)
    fake.people['s=bob'] = ('uid-bob', 4242, 'ИКБО-01-24')
    app = create_app(tmp_path, fake, FakeLogins())
    with TestClient(app) as client:
        yield client, app, fake, tmp_path

def sign_in(client, cookie='s=alice', device='Pixel'):
    return client.post('/v1/auth/mirea', json={'pulse_cookie': cookie, 'device': device})

def bearer(result):
    return {'Authorization': 'Bearer ' + result['access_token']}

def test_one_aa_id_per_mirea_person(setup):
    client, app, _, _ = setup
    first = sign_in(client).json()
    again = sign_in(client, 's=alice2', 'Tablet').json()
    bob = sign_in(client, 's=bob').json()
    assert first['user']['id'] == again['user']['id'] != bob['user']['id']
    # A later sign-in without a group keeps the known group.
    assert again['user']['group_id'] == 4242
    rows = app.state.store.db.execute('SELECT COUNT(*) FROM users').fetchone()[0]
    assert rows == 2
    with pytest.raises(Exception):
        app.state.store.db.execute("INSERT INTO mirea_links VALUES ('x', 'uid-alice', NULL, NULL, 0, 0)")

def test_invalid_mirea_session_is_rejected_and_cookie_not_stored(setup, tmp_path):
    client, _, _, directory = setup
    assert sign_in(client, 's=nobody').status_code == 401
    assert sign_in(client, 'bad cookie').status_code == 400
    sign_in(client)
    assert b's=alice' not in (directory / 'aaid.sqlite3').read_bytes()

def test_access_token_is_verifiable_offline(setup):
    client, app, _, _ = setup
    result = sign_in(client).json()
    jwks = client.get('/.well-known/jwks.json').json()
    keys = {key['kid']: unb64(key['x']) for key in jwks['keys']}
    claims = verify(result['access_token'], keys, jwks['issuer'], jwks['audience'])
    assert claims['mirea'] == 'uid-alice' and claims['grp'] == 4242 and claims['sub'] == result['user']['id']
    assert verify(result['access_token'], keys, jwks['issuer'], 'other') is None
    assert verify(result['access_token'], keys, jwks['issuer'], jwks['audience'], now=claims['exp']) is None
    head, payload, signature = result['access_token'].split('.')
    assert verify(f'{head}.{payload}x.{signature}', keys, jwks['issuer'], jwks['audience']) is None
    assert client.get('/v1/me', headers=bearer(result)).json()['id'] == result['user']['id']
    assert client.get('/v1/me').status_code == 401

def test_refresh_rotates_and_reuse_revokes_session(setup):
    client, _, _, _ = setup
    first = sign_in(client).json()
    second = client.post('/v1/auth/refresh', json={'refresh_token': first['refresh_token']})
    assert second.status_code == 200
    second = second.json()
    assert second['refresh_token'] != first['refresh_token']
    # Someone replays the old token: the whole device session dies.
    assert client.post('/v1/auth/refresh', json={'refresh_token': first['refresh_token']}).status_code == 401
    assert client.post('/v1/auth/refresh', json={'refresh_token': second['refresh_token']}).status_code == 401
    assert client.get('/v1/me', headers=bearer(second)).status_code == 401

def test_device_limit_evicts_least_recent_session(setup):
    client, _, _, _ = setup
    one = sign_in(client, device='one').json()
    two = sign_in(client, 's=alice2', 'two').json()
    three = sign_in(client, device='three').json()
    assert client.get('/v1/me', headers=bearer(one)).status_code == 401
    listed = client.get('/v1/me/sessions', headers=bearer(three)).json()
    assert [s['device'] for s in listed['sessions']] == ['three', 'two']
    assert client.delete('/v1/me/sessions/' + two['session_id'], headers=bearer(three)).status_code == 200
    assert client.get('/v1/me', headers=bearer(two)).status_code == 401
    assert client.post('/v1/auth/logout', headers=bearer(three)).status_code == 200
    assert client.get('/v1/me', headers=bearer(three)).status_code == 401

def test_sessions_are_private_to_owner(setup):
    client, _, _, _ = setup
    alice = sign_in(client).json()
    bob = sign_in(client, 's=bob').json()
    assert client.delete('/v1/me/sessions/' + alice['session_id'], headers=bearer(bob)).status_code == 404
    assert client.get('/v1/me', headers=bearer(alice)).status_code == 200

def test_blocked_user_cannot_sign_in(setup):
    client, _, _, _ = setup
    alice = sign_in(client).json()
    path = '/v1/admin/users/' + alice['user']['id'] + '/status'
    assert client.put(path, json={'status': 'blocked'}).status_code == 403
    assert client.put(path, json={'status': 'blocked'}, headers={'x-admin-token': 'a' * 32}).status_code == 200
    assert client.get('/v1/me', headers=bearer(alice)).status_code == 401
    assert sign_in(client).status_code == 403
    assert client.post('/v1/auth/refresh', json={'refresh_token': alice['refresh_token']}).status_code == 401

def grpc(message, status=0):
    return pulse.frame(message) + pulse.frame(f'grpc-status: {status}\r\n'.encode(), 128)

def field(number, value):
    return pulse.varint((number << 3) | 2) + pulse.varint(len(value)) + value

def test_pulse_reply_parsing():
    headers = {'content-type': 'application/grpc-web+proto'}
    me = field(1, field(1, field(1, b'uid-1')))
    assert pulse.profile_uid(200, headers, grpc(me)) == 'uid-1'
    assert pulse.profile_uid(200, headers, grpc(field(2, b''))) is None
    assert pulse.profile_uid(200, headers, grpc(b'', 16)) is None
    assert pulse.profile_uid(302, {}, b'') is None
    target = pulse.varint(8) + pulse.varint(1) + pulse.varint(16) + pulse.varint(777)
    entry = field(1, target) + field(2, 'БСБО-02-23'.encode()) + pulse.varint(32) + pulse.varint(1)
    teacher = field(1, pulse.varint(8) + pulse.varint(2) + pulse.varint(16) + pulse.varint(5))
    assert pulse.subscribed_group(headers, grpc(field(1, teacher) + field(1, entry))) == (777, 'БСБО-02-23')
    assert pulse.subscribed_group(headers, grpc(b'')) == (None, None)

def web_sign_in(client, app, identity=('uid-alice', 4242, 'ИКБО-01-24')):
    started = client.post('/web/logins').json()
    headers = {'X-Login-Token': started['token']}
    state = client.get('/web/logins/' + started['id'], headers=headers).json()
    assert state['status'] == 'login' and state['qr_png'].startswith('data:image/png;base64,')
    assert client.post('/web/logins/' + started['id'] + '/claim', headers=headers).status_code == 409
    app.state.logins.approve(started['id'], identity)
    claimed = client.post('/web/logins/' + started['id'] + '/claim', headers=headers)
    assert claimed.status_code == 200
    assert 'httponly' in claimed.headers['set-cookie'].lower() and 'samesite=strict' in claimed.headers['set-cookie'].lower()
    # A finished login can be claimed only once.
    assert client.post('/web/logins/' + started['id'] + '/claim', headers=headers).status_code == 409
    return started

def test_site_sign_in_shares_the_aa_id_with_the_app(setup):
    client, app, _, _ = setup
    assert client.get('/').status_code == 200 and 'AA ID' in client.get('/').text
    assert client.get('/static/aaid.js').status_code == 200
    assert client.get('/web/me').status_code == 401
    phone = sign_in(client).json()
    web_sign_in(client, app)
    me = client.get('/web/me').json()
    assert me['id'] == phone['user']['id'] and me['group_name'] == 'ИКБО-01-24'
    kinds = sorted(s['kind'] for s in me['sessions'])
    assert kinds == ['app', 'web']
    # The site can sign a phone out.
    assert client.delete('/web/sessions/' + phone['session_id']).status_code == 200
    assert client.get('/v1/me', headers=bearer(phone)).status_code == 401
    # A web session never works as an app token or refresh token.
    assert client.post('/web/logout').status_code == 200
    assert client.get('/web/me').status_code == 401

def test_site_login_token_is_required(setup):
    client, _, _, _ = setup
    started = client.post('/web/logins').json()
    assert client.get('/web/logins/' + started['id']).status_code == 404
    assert client.get('/web/logins/' + started['id'], headers={'X-Login-Token': 'x' * 31}).status_code == 404
    assert client.post('/web/logins', headers={'Origin': 'https://evil.example'}).status_code == 403

def test_one_browser_at_a_time_and_phones_unaffected(setup):
    client, app, _, _ = setup
    phone = sign_in(client).json()
    web_sign_in(client, app)
    first_cookie = client.cookies.get('aaid_web')
    client.cookies.clear()
    web_sign_in(client, app)
    assert client.get('/web/me').status_code == 200
    client.cookies.clear()
    client.cookies.set('aaid_web', first_cookie)
    assert client.get('/web/me').status_code == 401
    assert client.get('/v1/me', headers=bearer(phone)).status_code == 200
