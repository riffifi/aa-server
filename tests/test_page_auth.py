import time
import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app.pulse import LoginJob
from app.vault import digest

PASSWORD = 'a-test-password-with-enough-characters'


class FakePulse:
    async def identity(self, cookie):
        return 'valid', 'u'
    async def rpc(self, *a, **k):
        return None
    async def close(self):
        pass


@pytest.fixture
def app(tmp_path):
    return create_app(tmp_path, PASSWORD, FakePulse(), background=False)


def test_first_visit_sets_password_and_unlocks(app):
    with TestClient(app) as client:
        row = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})
        aid = row['id']
        state = client.get(f'/api/accounts/{aid}/page').json()
        assert state == {'authed': False, 'has_password': False, 'id': aid, 'label': 'Me'}
        # Detail and pass toggle are closed before auth.
        assert client.get(f'/api/accounts/{aid}').status_code == 401
        assert client.put(f'/api/accounts/{aid}/pass-allowed', json={'allowed': False}).status_code == 401
        # First visit sets the password and signs in.
        assert client.post(f'/api/accounts/{aid}/page/password', json={'password': 'hunter2'}).json()['authed'] is True
        page = client.get(f'/api/accounts/{aid}/page').json()
        assert page['authed'] is True and page['has_password'] is True
        assert client.get(f'/api/accounts/{aid}').status_code == 200
        assert client.put(f'/api/accounts/{aid}/pass-allowed', json={'allowed': False, 'expected_version': page['version']}).status_code == 200


def test_second_client_needs_the_password(app):
    with TestClient(app) as owner, TestClient(app) as stranger:
        aid = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})['id']
        owner.post(f'/api/accounts/{aid}/page/password', json={'password': 'hunter2'})
        # A different client without the cookie cannot set a new password or read detail.
        assert stranger.get(f'/api/accounts/{aid}/page').json() == {'authed': False, 'has_password': True, 'id': aid, 'label': 'Me'}
        assert stranger.post(f'/api/accounts/{aid}/page/password', json={'password': 'other'}).status_code == 403
        assert stranger.get(f'/api/accounts/{aid}').status_code == 401
        assert stranger.post(f'/api/accounts/{aid}/page/login', json={'password': 'wrong'}).status_code == 401
        assert stranger.post(f'/api/accounts/{aid}/page/login', json={'password': 'hunter2'}).status_code == 200
        assert stranger.get(f'/api/accounts/{aid}').status_code == 200


def test_logout_closes_the_session(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})['id']
        client.post(f'/api/accounts/{aid}/page/password', json={'password': 'hunter2'})
        assert client.get(f'/api/accounts/{aid}').status_code == 200
        client.post(f'/api/accounts/{aid}/page/logout')
        assert client.get(f'/api/accounts/{aid}').status_code == 401


def test_claim_grants_session_after_completed_login(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})['id']
        job = LoginJob('job1', digest('tok'), 'Me', None, time.time())
        job.status, job.result_id = 'complete', aid
        app.state.service.browser.jobs['job1'] = job
        claimed = client.post('/enroll/job1/tok/claim')
        assert claimed.status_code == 200 and claimed.json()['account_id'] == aid
        assert client.get(f'/api/accounts/{aid}').status_code == 200
        # Not-complete job cannot be claimed.
        job.status = 'login'
        with TestClient(app) as other:
            assert other.post('/enroll/job1/tok/claim').status_code == 409
