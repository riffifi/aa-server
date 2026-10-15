import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app import protocol

PASSWORD = 'a-test-password-with-enough-characters'
GRPC = {'content-type': 'application/grpc-web+proto'}


class FakePulse:
    def __init__(self):
        self.logged_out = False
    async def identity(self, cookie):
        return 'valid', cookie.split('=', 1)[1] if '=' in cookie else 'u'
    async def rpc(self, *a, **k):
        return None
    async def logout(self, cookie):
        self.logged_out = True
    async def close(self):
        pass


def unary():
    return protocol.frame(b'')


@pytest.fixture
def setup(tmp_path):
    pulse = FakePulse()
    app = create_app(tmp_path, PASSWORD, pulse, background=False)
    with TestClient(app) as client:
        yield client, app.state.service, pulse


def session(service, client, account_id):
    client.cookies.set('page_session', service.vault.create_page_session(account_id))


def test_end_session_clears_cookie_and_marks_expired(setup):
    client, service, pulse = setup
    row = service.vault.upsert('owner', 'Me', {'cookie': 'session=owner', 'storage_state': {'cookies': [], 'origins': []}})
    aid = row['id']
    session(service, client, aid)
    result = client.post(f'/api/accounts/{aid}/end-session')
    assert result.status_code == 200 and result.json()['state'] == 'expired'
    assert pulse.logged_out is True
    secret = service.vault.secret(service.vault.account(aid))
    assert 'cookie' not in secret and 'storage_state' not in secret


def test_end_session_requires_auth_and_blocks_rpc_after(setup):
    client, service, pulse = setup
    aid = service.vault.upsert('owner', 'Me', {'cookie': 'session=owner'})['id']
    # Not signed in -> refused.
    assert client.post(f'/api/accounts/{aid}/end-session').status_code == 401
    session(service, client, aid)
    assert client.post(f'/api/accounts/{aid}/end-session').status_code == 200
    # RPC proxy now refuses because the session is gone.
    assert client.post(f'/api/accounts/{aid}/rpc/attendance', content=unary(), headers=GRPC).status_code == 409


def test_panel_end_session_needs_password(setup):
    client, service, _ = setup
    aid = service.vault.upsert('owner', 'Me', {'cookie': 'session=owner'})['id']
    assert client.post(f'/panel/api/accounts/{aid}/end-session').status_code == 401
    csrf = client.post('/panel/api/login', json={'password': PASSWORD}).json()['csrf']
    assert client.post(f'/panel/api/accounts/{aid}/end-session', headers={'X-CSRF-Token': csrf}).status_code == 200


def test_upstream_logout_failure_still_revokes_locally_and_keeps_account_metadata(setup):
    client, service, pulse = setup
    row = service.vault.upsert('owner', 'Me', {'cookie': 'session=owner', 'storage_state': {'cookies': [], 'origins': []}}, group_id=123, group_name='My group')
    identifier = row['id']
    service.vault.set_pass_allowed(identifier, False)
    service.issued_tokens[identifier]['old-token-hash'] = 9999999999
    service.renewal_attempts[identifier] = 123
    session(service, client, identifier)
    async def unavailable(cookie):
        raise RuntimeError('upstream unavailable')
    pulse.logout = unavailable
    response = client.post(f'/api/accounts/{identifier}/end-session')
    assert response.status_code == 200
    after = service.vault.account(identifier)
    assert after['label'] == 'Me' and after['group_id'] == 123 and not after['pass_allowed']
    assert after['state'] == 'expired'
    assert 'cookie' not in service.vault.secret(after)
    assert 'storage_state' not in service.vault.secret(after)
    assert identifier not in service.issued_tokens
    assert identifier not in service.renewal_attempts
    assert client.post(f'/api/accounts/{identifier}/end-session').status_code == 200
    assert client.post(f'/api/accounts/{identifier}/rpc/profile', content=unary(), headers=GRPC).status_code == 409


@pytest.mark.asyncio
async def test_end_session_serializes_with_an_in_flight_refresh(tmp_path):
    import asyncio
    import httpx
    pulse = FakePulse()
    app = create_app(tmp_path, PASSWORD, pulse, background=False)
    service = app.state.service
    identifier = service.vault.upsert('owner', 'Me', {'cookie': 'session=owner'})['id']
    started, release = asyncio.Event(), asyncio.Event()
    calls = 0
    async def identity(cookie):
        nonlocal calls
        calls += 1
        started.set()
        await release.wait()
        return 'valid', 'owner'
    pulse.identity = identity
    refresh = asyncio.create_task(service.refresh_account(identifier))
    await started.wait()
    token = service.vault.create_page_session(identifier)
    try:
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app), base_url='http://testserver', cookies={'page_session': token}) as client:
            ending = asyncio.create_task(client.post(f'/api/accounts/{identifier}/end-session'))
            try:
                await asyncio.sleep(.05)
                assert not ending.done()
                assert not pulse.logged_out
                release.set()
                await refresh
                assert (await ending).status_code == 200
                assert 'cookie' not in service.vault.secret(service.vault.account(identifier))
                await service.refresh_account(identifier)
                assert calls == 1
                assert service.vault.account(identifier)['state'] == 'expired'
            finally:
                release.set()
                await asyncio.gather(refresh, ending, return_exceptions=True)
    finally:
        release.set()
        await service.close()
