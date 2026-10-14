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


def session_cookie(app, client, account_id):
    # A session is normally obtained by signing in via the university (claim);
    # tests mint one directly to stand in for that proven-ownership step.
    token = app.state.service.vault.create_page_session(account_id)
    client.cookies.set('page_session', token)
    return token


def test_passwordless_account_cannot_be_hijacked(app):
    with TestClient(app) as client:
        row = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})
        aid = row['id']
        state = client.get(f'/api/accounts/{aid}/page').json()
        assert state == {'authed': False, 'has_password': False, 'id': aid, 'label': 'Me'}
        # Detail, pass toggle and (critically) setting the first password are all
        # refused without a session — a stranger cannot claim a passwordless account.
        assert client.get(f'/api/accounts/{aid}').status_code == 401
        assert client.put(f'/api/accounts/{aid}/pass-allowed', json={'allowed': False}).status_code == 401
        assert client.post(f'/api/accounts/{aid}/page/password', json={'password': 'hunter2'}).status_code == 401


def test_setting_password_after_claimed_session(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})['id']
        session_cookie(app, client, aid)  # stands in for a completed university sign-in
        assert client.get(f'/api/accounts/{aid}').status_code == 200
        assert client.post(f'/api/accounts/{aid}/page/password', json={'password': 'hunter2'}).json()['authed'] is True
        # Drop the session to act as a fresh visitor: now the password is required.
        client.cookies.clear()
        assert client.get(f'/api/accounts/{aid}/page').json()['has_password'] is True
        assert client.get(f'/api/accounts/{aid}').status_code == 401
        assert client.post(f'/api/accounts/{aid}/page/login', json={'password': 'wrong'}).status_code == 401
        assert client.post(f'/api/accounts/{aid}/page/login', json={'password': 'hunter2'}).status_code == 200
        assert client.get(f'/api/accounts/{aid}').status_code == 200


def test_logout_closes_the_session(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})['id']
        session_cookie(app, client, aid)
        assert client.get(f'/api/accounts/{aid}').status_code == 200
        client.post(f'/api/accounts/{aid}/page/logout')
        assert client.get(f'/api/accounts/{aid}').status_code == 401


def test_page_seen_indicator_in_panel(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Me', {'cookie': 'session=u'})['id']
        csrf = client.post('/panel/api/login', json={'password': PASSWORD}).json()['csrf']
        before = {a['id']: a for a in client.get('/panel/api/state').json()['accounts']}[aid]
        assert before['page_seen_at'] is None
        app.state.service.vault.create_page_session(aid)  # owner signs in to their page
        after = {a['id']: a for a in client.get('/panel/api/state').json()['accounts']}[aid]
        assert after['page_seen_at'] is not None


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
