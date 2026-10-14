import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app import protocol

PASSWORD = 'a-test-password-with-enough-characters'
GRPC = {'content-type': 'application/grpc-web+proto'}
f = protocol.field


def test_acs_request_matches_the_captured_shape():
    body = protocol.acs_timeline_request('07dae93a-1741-4ab0-bc64-f68874669202', 1791925200, 1792011599)
    tree = protocol.decode_tree(body[5:])
    assert tree == {'1': '07dae93a-1741-4ab0-bc64-f68874669202',
                    '2': {'1': {'1': 1791925200}, '2': {'1': 1792011599, '2': 999000000}},
                    '3': 1, '4': 2}


def test_acs_events_parses_entries_exits_and_inside():
    # Item keyed 2 = entry (вход), keyed 1 = exit (выход).
    def event(ts, zone):
        return f(1, b'uuid') + f(2, protocol._timestamp(ts)) + f(3, f(1, b'z') + f(2, zone.encode())) + protocol._vfield(5, 1)
    message = (f(1, f(1, event(1000, 'Выход ЦКПП')))     # exit
               + f(1, f(2, event(2000, 'Вход')))           # entry (later)
               + f(1, f(1, protocol._vfield(2, 12345))))    # trailing summary -> ignored
    body = protocol.frame(message) + protocol.frame(b'grpc-status: 0\r\n', 128)
    events, inside = protocol.acs_events(body, GRPC)
    assert events == [{'direction': 'out', 'time': 1000, 'zone': 'Выход ЦКПП'},
                      {'direction': 'in', 'time': 2000, 'zone': 'Вход'}]
    assert inside is True  # last event is an entry

    only_exit = protocol.frame(f(1, f(1, event(1000, 'Выход')))) + protocol.frame(b'grpc-status: 0\r\n', 128)
    events, inside = protocol.acs_events(only_exit, GRPC)
    assert inside is False and events[0]['direction'] == 'out'


class FakePulse:
    def __init__(self):
        self.captured = None
    async def identity(self, cookie):
        return 'valid', 'u'
    async def rpc(self, *a, **k):
        return None
    async def acs_timeline(self, cookie, uid, start, end):
        self.captured = (uid, start, end)
        return [{'direction': 'in', 'time': start + 100, 'zone': 'ЦКПП'}], True
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
        assert data['inside'] is True and data['events'][0]['zone'] == 'ЦКПП'
        assert pulse.captured[0] == 'person-123'  # queried by the account's pulse id
