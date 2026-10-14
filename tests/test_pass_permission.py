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
    def __init__(self):
        self.state = 'valid'
    async def identity(self, cookie):
        return self.state, cookie.split('=', 1)[1]
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


def test_pass_defaults_on_and_single_account_endpoint(setup):
    client, service = setup
    row = service.vault.upsert('person', 'Person', {'cookie': 'session=person'})
    detail = client.get('/api/accounts/' + row['id'])
    assert detail.status_code == 200 and detail.json()['pass_allowed'] is True
    assert client.get('/api/accounts/missing').status_code == 404
    page = client.get('/u/' + row['id'])
    assert page.status_code == 200 and 'Your account' in page.text


def test_toggling_permission_blocks_pass_rpcs_only(setup):
    client, service = setup
    row = service.vault.upsert('person', 'Person', {'cookie': 'session=person'})
    # Default on: a pass RPC is not blocked by the permission gate.
    assert client.post(f'/api/accounts/{row["id"]}/rpc/pass-token', content=unary(), headers=GRPC).status_code != 403
    put = client.put(f'/api/accounts/{row["id"]}/pass-allowed', json={'allowed': False, 'expected_version': row['version']})
    assert put.status_code == 200 and put.json()['pass_allowed'] is False
    # Now every pass method is refused, but non-pass RPCs still work.
    for method in ('pass-token', 'pass-code', 'pass-issue'):
        assert client.post(f'/api/accounts/{row["id"]}/rpc/{method}', content=unary(), headers=GRPC).status_code == 403
    assert client.post(f'/api/accounts/{row["id"]}/rpc/profile', content=unary(), headers=GRPC).status_code != 403
    # Re-enabling lifts the block.
    current = client.get('/api/accounts/' + row['id']).json()['version']
    client.put(f'/api/accounts/{row["id"]}/pass-allowed', json={'allowed': True, 'expected_version': current})
    assert client.post(f'/api/accounts/{row["id"]}/rpc/pass-token', content=unary(), headers=GRPC).status_code != 403


def test_permission_change_emits_sync_event(setup):
    client, service = setup
    row = service.vault.upsert('person', 'Person', {'cookie': 'session=person'})
    cursor = client.get('/api/events?after=0&wait=0').json()['cursor']
    client.put(f'/api/accounts/{row["id"]}/pass-allowed', json={'allowed': False, 'expected_version': row['version']})
    events = client.get(f'/api/events?after={cursor}&wait=0').json()['events']
    assert any(event['kind'] == 'pass_permission' for event in events)


def test_permission_toggle_needs_no_panel_password(setup):
    client, _ = setup
    assert client.get('/panel/api/state').status_code == 401  # not signed in
    # but panel variant does require it
    row_resp = client.post('/api/accounts/import', json={'label': 'Imp', 'cookie': 'session=imported'})
    account_id = row_resp.json()['id']
    assert client.put(f'/panel/api/accounts/{account_id}/pass-allowed', json={'allowed': False}).status_code == 401
    assert client.put(f'/api/accounts/{account_id}/pass-allowed', json={'allowed': False}).status_code == 200
