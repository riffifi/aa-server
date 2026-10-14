import pytest
from fastapi.testclient import TestClient
from app.main import create_app

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


def session(app, client, account_id):
    client.cookies.set('page_session', app.state.service.vault.create_page_session(account_id))


def test_login_without_label_keeps_existing_name(app):
    vault = app.state.service.vault
    row = vault.upsert('u', 'Иван Петров', {'cookie': 'session=u'})
    # A university sign-in sends no label; it must not rename the account.
    updated = vault.upsert('u', None, {'cookie': 'session=u2'}, account_id=row['id'], session_expected=row['session_version'])
    assert updated['label'] == 'Иван Петров'


def test_rename_from_own_page_session(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Old', {'cookie': 'session=u'})['id']
        # No session -> refused.
        assert client.put(f'/api/accounts/{aid}/label', json={'label': 'New'}).status_code == 401
        session(app, client, aid)
        result = client.put(f'/api/accounts/{aid}/label', json={'label': 'New'})
        assert result.status_code == 200 and result.json()['label'] == 'New'


def test_rename_from_panel(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Old', {'cookie': 'session=u'})['id']
        assert client.put(f'/panel/api/accounts/{aid}/label', json={'label': 'New'}).status_code == 401
        csrf = client.post('/panel/api/login', json={'password': PASSWORD}).json()['csrf']
        result = client.put(f'/panel/api/accounts/{aid}/label', json={'label': 'Админ назвал'}, headers={'X-CSRF-Token': csrf})
        assert result.status_code == 200 and result.json()['label'] == 'Админ назвал'


def test_rename_rejects_empty_and_emits_event(app):
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('u', 'Old', {'cookie': 'session=u'})['id']
        session(app, client, aid)
        assert client.put(f'/api/accounts/{aid}/label', json={'label': '   '}).status_code == 400
        cursor = client.get('/api/events?after=0&wait=0').json()['cursor']
        client.put(f'/api/accounts/{aid}/label', json={'label': 'Renamed'})
        events = client.get(f'/api/events?after={cursor}&wait=0').json()['events']
        assert any(e['kind'] == 'label' for e in events)


def test_login_page_is_served(app):
    with TestClient(app) as client:
        page = client.get('/login')
        assert page.status_code == 200 and 'Вход через университет' in page.text
