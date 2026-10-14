import json
import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app.pulse import Reply
from app import protocol

PASSWORD = 'a-test-password-with-enough-characters'
GRPC = {'content-type': 'application/grpc-web+proto'}


def profile_body():
    user = protocol.field(1, b'student-1') + protocol.field(3, 'ИНБО-10-23'.encode())
    authorized = protocol.field(1, user)
    outer = protocol.field(1, authorized)
    return protocol.frame(outer) + protocol.frame(b'grpc-status: 0\r\n', 128)


class FakePulse:
    async def identity(self, cookie):
        return 'valid', 'student-1'
    async def rpc(self, cookie, method, body, authorization=None):
        return Reply(200, GRPC, profile_body())
    async def close(self):
        pass


def test_decode_tree_handles_nested_strings_and_varints():
    raw = protocol.field(1, 'ИНБО-10-23'.encode()) + protocol.varint((3 << 3)) + protocol.varint(42)
    tree = protocol.decode_tree(raw)
    assert tree['1'] == 'ИНБО-10-23'
    assert tree['3'] == 42


def test_decode_tree_never_raises_on_garbage():
    assert isinstance(protocol.decode_tree(b'\xff\xff\xff\x7f\x7f'), dict)


@pytest.fixture
def client(tmp_path):
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(app) as test_client:
        yield test_client, app.state.service


def test_profile_fields_requires_panel_and_returns_tree(client):
    test_client, service = client
    row = service.vault.upsert('student-1', 'Student', {'cookie': 'session=student-1'})
    # Not signed in to the panel -> refused.
    assert test_client.get(f'/panel/api/accounts/{row["id"]}/profile-fields').status_code == 401
    test_client.post('/panel/api/login', json={'password': PASSWORD})
    response = test_client.get(f'/panel/api/accounts/{row["id"]}/profile-fields')
    assert response.status_code == 200
    data = response.json()
    assert data['grpc_status'] == 0
    assert 'ИНБО-10-23' in json.dumps(data['fields'], ensure_ascii=False)
