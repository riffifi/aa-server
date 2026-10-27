from test_server import site_session
import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app.pulse import Reply
from app import protocol

PASSWORD = 'a-test-password-with-enough-characters'
GRPC = {'content-type': 'application/grpc-web+proto'}


def reply():
    return Reply(200, GRPC, protocol.frame(b'') + protocol.frame(b'grpc-status: 0\r\n', 128))


class FakePulse:
    async def identity(self, cookie):
        return 'valid', cookie.split('=', 1)[1]
    async def rpc(self, cookie, method, body, authorization=None):
        return reply()
    async def close(self):
        pass


def unary():
    return protocol.frame(b'')


@pytest.fixture
def setup(tmp_path):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(app) as client:
        yield client, app.state.service


def signin(client, service, account_id):
    # A page session (normally from a university sign-in) unlocks detail and the toggle.
    client.cookies.set('aa_site', site_session(service.vault, account_id))


def test_pass_defaults_off_and_single_account_endpoint(setup):
    client, service = setup
    row = service.vault.upsert('person', 'Person', {'cookie': 'session=person'})
    signin(client, service, row["id"])
    detail = client.get('/api/accounts/' + row['id'])
    assert detail.status_code == 200 and detail.json()['pass_allowed'] is False
    assert client.get('/api/accounts/missing').status_code == 404
    # The 1.x owner page now leads to the site.
    page = client.get('/u/' + row['id'], follow_redirects=False)
    assert page.status_code == 301 and page.headers['location'] == '/'


def test_toggling_permission_blocks_pass_rpcs_only(setup):
    client, service = setup
    row = service.vault.upsert('person', 'Person', {'cookie': 'session=person'})
    signin(client, service, row["id"])
    assert client.post(f'/api/accounts/{row["id"]}/rpc/pass-token', content=unary(), headers=GRPC).status_code == 403
    on = client.put(f'/api/accounts/{row["id"]}/pass-allowed', json={'allowed': True, 'expected_version': row['version']}).json()
    assert client.post(f'/api/accounts/{row["id"]}/rpc/pass-token', content=unary(), headers=GRPC).status_code != 403
    put = client.put(f'/api/accounts/{row["id"]}/pass-allowed', json={'allowed': False, 'expected_version': on['version']})
    assert put.status_code == 200 and put.json()['pass_allowed'] is False
    for method in ('pass-token', 'pass-code', 'pass-issue'):
        assert client.post(f'/api/accounts/{row["id"]}/rpc/{method}', content=unary(), headers=GRPC).status_code == 403
    assert client.post(f'/api/accounts/{row["id"]}/rpc/profile', content=unary(), headers=GRPC).status_code != 403
    current = client.get('/api/accounts/' + row['id']).json()['version']
    client.put(f'/api/accounts/{row["id"]}/pass-allowed', json={'allowed': True, 'expected_version': current})
    assert client.post(f'/api/accounts/{row["id"]}/rpc/pass-token', content=unary(), headers=GRPC).status_code != 403


def test_permission_change_emits_sync_event(setup):
    client, service = setup
    row = service.vault.upsert('person', 'Person', {'cookie': 'session=person'})
    signin(client, service, row["id"])
    cursor = client.get('/api/events?after=0&wait=0').json()['cursor']
    client.put(f'/api/accounts/{row["id"]}/pass-allowed', json={'allowed': True, 'expected_version': row['version']})
    events = client.get(f'/api/events?after={cursor}&wait=0').json()['events']
    assert any(event['kind'] == 'pass_permission' for event in events)


def test_pass_toggle_requires_page_session_or_panel(setup):
    client, service = setup
    account_id = service.vault.upsert('person', 'Person', {'cookie': 'session=person'})['id']
    # No page session and no panel password -> refused on both variants.
    assert client.put(f'/api/accounts/{account_id}/pass-allowed', json={'allowed': False}).status_code == 401
    assert client.put(f'/panel/api/accounts/{account_id}/pass-allowed', json={'allowed': False}).status_code == 401
    # With a page session the public variant works.
    signin(client, service, account_id)
    assert client.put(f'/api/accounts/{account_id}/pass-allowed', json={'allowed': False}).status_code == 200
