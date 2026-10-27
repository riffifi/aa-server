import base64
import json
import time
import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives import serialization
from fastapi.testclient import TestClient
from app.main import create_app
from app.aaid import Verifier
from test_server import FakePulse, PASSWORD, account

KEY = Ed25519PrivateKey.generate()
OTHER = Ed25519PrivateKey.generate()
RAW = KEY.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)

def b64(data):
    return base64.urlsafe_b64encode(data).rstrip(b'=').decode()

def token(mirea='person-one', key=KEY, kid='k1', **overrides):
    claims = {'iss': 'aa-id', 'aud': 'aa', 'sub': 'user-1', 'sid': 's-1', 'mirea': mirea, 'grp': None,
              'iat': int(time.time()), 'exp': int(time.time()) + 600, **overrides}
    head = b64(json.dumps({'alg': 'EdDSA', 'kid': kid}).encode())
    body = b64(json.dumps(claims).encode())
    return f'{head}.{body}.' + b64(key.sign(f'{head}.{body}'.encode()))

@pytest.fixture
def setup(tmp_path):
    verifier = Verifier(keys={'k1': RAW}, public_url='https://aa.example/id')
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False, aaid=verifier)
    with TestClient(app) as client:
        yield client, app.state.service

def auth(value):
    return {'Authorization': 'Bearer ' + value}

def test_app_api_requires_aa_id(setup):
    client, _ = setup
    health = client.get('/health').json()
    assert health['aaid'] == {'required': True, 'url': 'https://aa.example/id'}
    for path in ('/api/accounts', '/api/events?wait=0', '/api/groups'):
        response = client.get(path)
        assert response.status_code == 401, path
        assert response.headers['x-aaid-required'] == '1'
        assert response.json()['aaid_url'] == 'https://aa.example/id'
    assert client.get('/api/accounts', headers=auth(token())).status_code == 200

@pytest.mark.parametrize('bad', [
    token(key=OTHER), token(kid='unknown'), token(exp=int(time.time()) - 1), token(aud='other'),
    token(iss='someone'), 'not-a-token', token()[:-4] + 'AAAA'])
def test_invalid_tokens_are_rejected(setup, bad):
    client, _ = setup
    assert client.get('/api/accounts', headers=auth(bad)).status_code == 401

def test_rpc_and_uploads_need_aa_id(setup):
    client, service = setup
    row = account(service)
    frame = b'\x00\x00\x00\x00\x00'
    assert client.post(f'/api/accounts/{row["id"]}/rpc/profile', content=frame).status_code == 401
    assert client.post('/api/accounts/import', json={'cookie': 'session=x', 'label': 'X'}).status_code == 401
    assert client.put(f'/api/accounts/{row["id"]}/group', json={'group_id': 1, 'expected_version': row['version']}).status_code == 401
    assert client.post(f'/api/accounts/{row["id"]}/rpc/profile', content=frame, headers=auth(token())).status_code == 200

def test_own_account_is_marked(setup):
    client, service = setup
    account(service, 'person-one')
    account(service, 'person-two')
    listed = client.get('/api/accounts', headers=auth(token('person-two'))).json()['accounts']
    assert sorted((a['own'] for a in listed)) == [False, True]

def test_browser_pages_keep_working_without_aa_id(setup):
    client, service = setup
    row = account(service)
    assert client.get(f'/api/accounts/{row["id"]}/page').status_code == 200
    assert client.get('/api/groups/search?q=a').status_code == 200
    # Without a page session the owner endpoints still demand AA ID.
    assert client.get(f'/api/accounts/{row["id"]}').status_code == 401
    session = service.vault.create_page_session(row['id'])
    client.cookies.set('page_session', session)
    assert client.get(f'/api/accounts/{row["id"]}').status_code == 200
    # A page session for one account does not open another account or the shared list.
    other = account(service, 'person-two')
    assert client.get(f'/api/accounts/{other["id"]}').status_code == 401
    assert client.get('/api/accounts').status_code == 401

def test_server_without_aa_id_stays_open(tmp_path):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(app) as client:
        assert client.get('/health').json()['aaid'] == {'required': False}
        assert client.get('/api/accounts').status_code == 200

class AcsPulse(FakePulse):
    windows = ()
    async def acs_timeline(self, cookie, uid, start, end):
        self.windows += ((uid, start, end),)
        if len(self.windows) != 2:  # passes only on one of the days
            return [], False
        return [{'direction': 'in', 'time': 100, 'zone': 'Вход'}, {'direction': 'out', 'time': 300, 'zone': 'Выход'},
                {'direction': 'in', 'time': 200, 'zone': 'Вход'}], False

def test_own_attendance_for_the_aa_id_person(tmp_path):
    pulse = AcsPulse()
    verifier = Verifier(keys={'k1': RAW}, enforce=False)
    app = create_app(tmp_path, PASSWORD, pulse, background=False, aaid=verifier)
    with TestClient(app) as client:
        service = app.state.service
        account(service, 'person-one')
        account(service, 'person-two')
        # Not enforcing: the app API stays open, but /api/me needs a token.
        assert client.get('/api/accounts').status_code == 200
        assert client.get('/health').json()['aaid']['required'] is False
        assert client.get('/api/me/attendance').status_code == 401
        result = client.get('/api/me/attendance', headers=auth(token('person-two'))).json()
        # Pulse is asked one day at a time, newest day first.
        assert len(pulse.windows) == 7 and {w[0] for w in pulse.windows} == {'person-two'}
        assert all(end - start == 86399 for _, start, end in pulse.windows)
        assert pulse.windows[0][1] - pulse.windows[1][1] == 86400
        assert [e['time'] for e in result['events']] == [300, 200, 100]
        assert result['last_in']['time'] == 200 and result['last_out']['time'] == 300
        assert client.get('/api/me/attendance', headers=auth(token('nobody'))).status_code == 404
