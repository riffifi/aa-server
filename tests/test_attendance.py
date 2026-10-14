import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app import protocol

PASSWORD = 'a-test-password-with-enough-characters'
GRPC = {'content-type': 'application/grpc-web+proto'}
f = protocol.field
ts = protocol._timestamp
vf = protocol._vfield


def event(t, code, zone):
    return (f(1, b'uuid') + f(2, ts(t)) + f(3, f(1, b'z') + f(2, zone.encode()))
            + f(4, f(1, b'd') + f(2, 'Неконтролируемая территория'.encode())) + vf(5, code))


def timeline(*items):
    return protocol.frame(b''.join(f(1, it) for it in items)) + protocol.frame(b'grpc-status: 0\r\n', 128)


def test_acs_request_matches_the_captured_shape():
    body = protocol.acs_timeline_request('07dae93a-1741-4ab0-bc64-f68874669202', 1791925200, 1792011599)
    assert protocol.decode_tree(body[5:]) == {'1': '07dae93a-1741-4ab0-bc64-f68874669202',
        '2': {'1': {'1': 1791925200}, '2': {'1': 1792011599, '2': 999000000}}, '3': 1, '4': 2}


def test_acs_events_direction_by_code_and_inside():
    # Mirrors real nesting: checkpoint code 3 (ignored), inner gate code 1 = in, code 2 = out.
    body = timeline(
        f(1, event(1000, 3, 'В78 ЦКПП')),          # ignored
        f(2, event(2000, 1, 'ЦентральныйВход')),    # entry
        f(2, event(3000, 2, 'Выход')),              # exit
        f(1, vf(2, 999)),                           # summary double -> no event
    )
    events, inside = protocol.acs_events(body, GRPC)
    assert events == [{'direction': 'in', 'time': 2000, 'zone': 'ЦентральныйВход'},
                      {'direction': 'out', 'time': 3000, 'zone': 'Выход'}]
    assert inside is False

    still_in = timeline(f(1, event(500, 3, 'ЦКПП')), f(2, event(1000, 1, 'Вход')))
    events, inside = protocol.acs_events(still_in, GRPC)
    assert inside is True and [e['direction'] for e in events] == ['in']


class FakePulse:
    def __init__(self):
        self.captured = None
    async def identity(self, cookie):
        return 'valid', 'u'
    async def rpc(self, *a, **k):
        return None
    async def acs_timeline(self, cookie, uid, start, end):
        self.captured = (uid, start, end)
        return [{'direction': 'in', 'time': start + 100, 'zone': 'Вход'}], True
    async def close(self):
        pass


def test_attendance_endpoint_requires_session(tmp_path):
    pulse = FakePulse()
    app = create_app(tmp_path, PASSWORD, pulse, background=False)
    with TestClient(app) as client:
        aid = app.state.service.vault.upsert('person-123', 'Me', {'cookie': 'session=x'})['id']
        assert client.get(f'/api/accounts/{aid}/attendance').status_code == 401
        client.cookies.set('page_session', app.state.service.vault.create_page_session(aid))
        data = client.get(f'/api/accounts/{aid}/attendance').json()
        assert data['inside'] is True and data['events'][0]['direction'] == 'in'
        assert pulse.captured[0] == 'person-123'
