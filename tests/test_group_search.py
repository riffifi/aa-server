from types import SimpleNamespace
from unittest.mock import AsyncMock
import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app.pulse import PulseTransport

PASSWORD = 'a-test-password-with-enough-characters'


class FakePulse:
    def __init__(self):
        self.groups = []
    async def identity(self, cookie):
        return 'valid', 'u'
    async def rpc(self, *a, **k):
        return None
    async def search_groups(self, query):
        return self.groups
    async def close(self):
        pass


@pytest.mark.asyncio
async def test_search_groups_keeps_only_groups_with_ids():
    transport = PulseTransport.__new__(PulseTransport)
    payload = {'data': [
        {'id': 123, 'targetTitle': 'ИНБО-10-23', 'fullTitle': 'ИНБО-10-23 (1 курс)', 'scheduleTarget': 1},
        {'id': 999, 'targetTitle': 'Иванов И.И.', 'scheduleTarget': 2},        # teacher, dropped
        {'targetTitle': 'no-id', 'scheduleTarget': 1},                          # no id, dropped
    ]}
    transport.http = SimpleNamespace(get=AsyncMock(return_value=SimpleNamespace(status_code=200, json=lambda: payload)))
    result = await transport.search_groups('инбо')
    assert result == [{'id': 123, 'name': 'ИНБО-10-23', 'full': 'ИНБО-10-23 (1 курс)'}]


@pytest.mark.asyncio
async def test_search_groups_tolerates_errors():
    transport = PulseTransport.__new__(PulseTransport)
    transport.http = SimpleNamespace(get=AsyncMock(return_value=SimpleNamespace(status_code=500, json=lambda: {})))
    assert await transport.search_groups('x') == []


@pytest.fixture
def client(tmp_path):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(app) as test_client:
        yield test_client, app.state.service


def test_group_search_endpoint(client):
    test_client, service = client
    service.transport.groups = [{'id': 123, 'name': 'ИНБО-10-23', 'full': 'ИНБО-10-23'}]
    assert test_client.get('/api/groups/search?q=и').json() == {'groups': []}  # too short
    data = test_client.get('/api/groups/search?q=инбо').json()
    assert data['groups'][0]['id'] == 123 and data['groups'][0]['name'] == 'ИНБО-10-23'
