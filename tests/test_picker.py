from types import SimpleNamespace
import pytest
from fastapi.testclient import TestClient
from app.main import create_app

PASSWORD = 'a-test-password-with-enough-characters'


class FakePulse:
    def __init__(self):
        self.state = 'valid'
    async def identity(self, cookie):
        return self.state, 'owner'
    async def rpc(self, *a, **k):
        return None
    async def close(self):
        pass


@pytest.fixture
def setup(tmp_path):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    service = app.state.service
    # Do not launch a real browser during these tests.
    service.browser.start = lambda label, account_id=None: (SimpleNamespace(id='job-1'), 'capability-token')
    with TestClient(app) as client:
        yield client, service


def test_root_serves_picker_and_panel_moved(setup):
    client, _ = setup
    root = client.get('/')
    assert root.status_code == 200 and 'Keep your account connected' in root.text
    panel = client.get('/panel')
    assert panel.status_code == 200 and 'Sessions, shared once' in panel.text


def test_public_login_start_requires_existing_account(setup):
    client, service = setup
    assert client.post('/api/logins', json={'label': 'Me', 'account_id': 'missing'}).status_code == 404
    row = service.vault.upsert('owner', 'Owner', {'cookie': 'session=owner'})
    good = client.post('/api/logins', json={'label': 'Owner', 'account_id': row['id']})
    assert good.status_code == 200
    assert good.json()['enrollment_path'] == '/enroll/job-1/capability-token'


def test_public_login_start_is_rate_limited(setup):
    client, _ = setup
    codes = [client.post('/api/logins', json={'label': 'New'}).status_code for _ in range(41)]
    assert codes[:40] == [200] * 40
    assert codes[40] == 429


def test_public_login_start_needs_no_panel_password(setup):
    client, _ = setup
    # The picker never signs in to the panel; starting a login must work anonymously.
    assert client.post('/api/logins', json={'label': 'Anon'}).status_code == 200
    assert client.get('/panel/api/state').status_code == 401
