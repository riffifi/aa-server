import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app import protocol

PASSWORD = 'a-test-password-with-enough-characters'
GRPC = {'content-type': 'application/grpc-web+proto'}
f = protocol.field
ts = protocol._timestamp
vf = protocol._vfield


def test_acs_request_matches_the_captured_shape():
    body = protocol.acs_stats_request('07dae93a-1741-4ab0-bc64-f68874669202', 1791925200, 1792011599)
    tree = protocol.decode_tree(body[5:])
    assert tree == {'1': '07dae93a-1741-4ab0-bc64-f68874669202',
                    '2': {'1': {'1': 1791925200}, '2': {'1': 1792011599, '2': 999000000}},
                    '3': 1}


def test_acs_sessions_pairs_entry_exit_and_inside():
    done = f(1, ts(1000)) + f(2, ts(2000)) + vf(4, 1)   # entered 1000, exited 2000
    still = f(1, ts(3000))                               # entered 3000, no exit -> inside
    message = vf(1, 2) + f(3, done) + f(3, still)
    body = protocol.frame(message) + protocol.frame(b'grpc-status: 0\r\n', 128)
    sessions, inside = protocol.acs_sessions(body, GRPC)
    assert sessions == [{'enter': 1000, 'exit': 2000}, {'enter': 3000, 'exit': None}]
    assert inside is True

    closed = protocol.frame(vf(1, 1) + f(3, f(1, ts(1000)) + f(2, ts(2000)))) + protocol.frame(b'grpc-status: 0\r\n', 128)
    sessions, inside = protocol.acs_sessions(closed, GRPC)
    assert inside is False and sessions == [{'enter': 1000, 'exit': 2000}]


class FakePulse:
    def __init__(self):
        self.captured = None
    async def identity(self, cookie):
        return 'valid', 'u'
    async def rpc(self, *a, **k):
        return None
    async def acs_stats(self, cookie, uid, start, end):
        self.captured = (uid, start, end)
        return [{'enter': start + 100, 'exit': None}], True
    async def close(self):
        pass


def test_attendance_endpoint_requires_session(tmp_path):
    pulse = FakePulse()
    app = create_app(tmp_path, PASSWORD, pulse, background=False)
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('person-123', 'Me', {'cookie': 'session=x'})['id']
        assert client.get(f'/api/accounts/{aid}/attendance').status_code == 401
        client.cookies.set('page_session', app.state.service.vault.create_page_session(aid))
        result = client.get(f'/api/accounts/{aid}/attendance')
        assert result.status_code == 200
        data = result.json()
        assert data['inside'] is True and data['sessions'][0]['exit'] is None
        assert pulse.captured[0] == 'person-123'
