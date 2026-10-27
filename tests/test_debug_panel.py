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
