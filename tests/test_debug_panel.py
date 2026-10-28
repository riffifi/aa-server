import logging
from test_server import setup, account, PASSWORD  # noqa: F401

def test_debug_tabs_need_the_panel_and_hide_secrets(setup):
    client, service = setup[0], setup[1]
    row = account(service)
    for path in ('logs', 'requests', 'audit', 'tables', 'tables/accounts', 'system'):
        assert client.get('/panel/api/debug/' + path).status_code == 401
    client.post('/panel/api/login', json={'password': PASSWORD})
    names = {t['name']: t for t in client.get('/panel/api/debug/tables').json()['tables']}
    assert {'accounts', 'audit_events', 'marks', 'pass_shares', 'panel_sessions'} <= set(names)
    accounts = client.get('/panel/api/debug/tables/accounts').json()
    record = dict(zip(accounts['columns'], accounts['rows'][0]))
    assert record['id'] == row['id'] and record['secret'] == '•••'
    sessions = client.get('/panel/api/debug/tables/panel_sessions').json()
    assert all(value == '•••' for value in sessions['rows'][0][:2])
    config = client.get('/panel/api/debug/tables/config').json()
    assert all(r[1] == '•••' for r in config['rows'])
    assert client.get('/panel/api/debug/tables/accounts?q=' + row['id'][:6]).json()['total'] == 1
    assert client.get('/panel/api/debug/tables/nope').status_code == 404
    assert client.get('/panel/api/debug/tables/accounts%22%20--').status_code == 404

def test_debug_logs_requests_and_system(setup):
    client = setup[0]
    client.post('/panel/api/login', json={'password': PASSWORD})
    logging.getLogger('app.test').warning('something odd %s', 42)
    try:
        raise ValueError('boom')
    except ValueError:
        logging.getLogger('app.test').exception('it broke')
    logs = client.get('/panel/api/debug/logs?level=warning').json()['logs']
    assert logs[0]['message'] == 'it broke' and 'ValueError: boom' in logs[0]['detail']
    assert any(r['message'] == 'something odd 42' for r in logs)
    newest = logs[0]['id']
    assert client.get(f'/panel/api/debug/logs?after={newest}').json()['logs'] == []
    client.get('/health')
    routes = {r['route']: r for r in client.get('/panel/api/debug/requests').json()['routes']}
    assert routes['GET /health']['count'] >= 1
    system = client.get('/panel/api/debug/system').json()
    assert system['background']['refresh_loop'] == 'off' and 'sessions.sqlite3' in system['files']

def test_pulse_recording_needs_a_saved_browser_session(setup):
    client, service = setup[0], setup[1]
    row = account(service)
    csrf = client.post('/panel/api/login', json={'password': PASSWORD}).json()['csrf']
    assert client.post('/panel/api/debug/pulse-record', json={'account_id': row['id']}).status_code == 403
    response = client.post('/panel/api/debug/pulse-record', json={'account_id': row['id']}, headers={'x-csrf-token': csrf})
    assert response.status_code == 409
    assert client.post('/panel/api/debug/pulse-record', json={'account_id': 'nope'}, headers={'x-csrf-token': csrf}).status_code == 404

def test_panel_answers_only_through_the_local_listener(tmp_path, monkeypatch):
    from fastapi.testclient import TestClient
    from app.main import create_app
    from test_server import FakePulse
    monkeypatch.setenv('PANEL_LOCAL_ONLY', '1')
    app = create_app(tmp_path, PASSWORD, FakePulse(), background=False)
    with TestClient(app) as client:
        assert client.get('/panel').status_code == 404
        assert client.post('/panel/api/login', json={'password': PASSWORD}).status_code == 404
        local = {'x-aa-panel': '1'}
        assert client.get('/panel', headers=local).status_code == 200
        login = client.post('/panel/api/login', json={'password': PASSWORD}, headers=local)
        assert login.status_code == 200 and 'secure' not in login.headers['set-cookie'].lower()
        assert client.get('/panel/api/state', headers=local).status_code == 200
        assert client.get('/panel/api/state').status_code == 404
        assert client.get('/health').status_code == 200
