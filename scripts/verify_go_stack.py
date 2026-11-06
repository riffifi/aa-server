"""Verify an explicitly selected local Go stack without calling MIREA.

Reads the test panel password from the named environment file. It never prints
credentials, changes accounts or starts a browser login. Run after Compose is up.
"""
import argparse
import http.cookiejar
import json
from pathlib import Path
import urllib.error
import urllib.request


def verify(env_file, site, identity, panel, panel_password=None):
    for origin in (site, identity, panel):
        from urllib.parse import urlsplit
        parsed = urlsplit(origin)
        if parsed.scheme != 'http' or parsed.hostname != '127.0.0.1' or parsed.path not in ('', '/'):
            raise ValueError('Verification accepts local http://127.0.0.1 origins only')
    settings = dict(line.split('=', 1) for line in Path(env_file).read_text().splitlines()
                    if line and not line.startswith('#') and '=' in line)
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar))
    checks = 0

    def request(origin, path, expected=200, body=None, headers=None):
        nonlocal checks
        headers = dict(headers or {})
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers['Content-Type'] = 'application/json'
        req = urllib.request.Request(origin + path, data=data, headers=headers)
        try:
            response = opener.open(req, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read(2 << 20)
            if response.status != expected:
                raise AssertionError(f'{path}: expected {expected}, got {response.status}')
            checks += 1
            return json.loads(raw) if 'application/json' in response.headers.get('Content-Type', '') else raw

    assert request(site, '/health')['ok'] is True
    assert request(identity, '/health')['service'] == 'aa-id'
    keys = request(identity, '/.well-known/jwks.json')['keys']
    assert len(keys) == 1 and keys[0]['kty'] == 'OKP' and 'd' not in keys[0]
    request(site, '/')
    request(identity, '/')
    request(site, '/id/health')
    for path in ('/api/accounts', '/api/events?wait=0', '/api/groups', '/site/state', '/site/today'):
        request(site, path, 401)
    request(identity, '/v1/me', 401)
    request(identity, '/unknown', 404, body={})
    request(identity, '/health', 405, body={})
    request(identity, '/v1/auth/refresh', 405)
    request(site, '/panel', 404)
    request(site, '/panel/api/state', 404, headers={'X-AA-Panel': '1'})
    request(site, '/telegram', 404)
    request(site, '/static/telegram.js', 404)
    request(panel, '/panel/api/state', 401)
    login = request(panel, '/panel/api/login', body={'password': panel_password if panel_password is not None else settings['GO_ADMIN_PASSWORD']})
    csrf = login['csrf']
    assert csrf and request(panel, '/panel/api/state')['csrf'] == csrf
    for path in ('/panel/api/pass-snapshot', '/panel/api/debug/browser', '/panel/api/debug/system',
                 '/panel/api/debug/tables', '/panel/api/debug/requests', '/panel/api/debug/logs'):
        request(panel, path)
    config = request(panel, '/panel/api/debug/tables/config')
    assert settings['GO_ADMIN_PASSWORD'] not in json.dumps(config)
    # The panel cookie must not bypass the application identity boundary.
    request(site, '/api/accounts', 401)
    request(panel, '/panel/api/logout', 403, body={})
    request(panel, '/panel/api/logout', body={}, headers={'X-CSRF-Token': csrf})
    request(panel, '/panel/api/state', 401)
    print(f'Local Go stack: {checks} HTTP checks passed; no external login performed.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--env-file', required=True)
    parser.add_argument('--site', default='http://127.0.0.1:18080')
    parser.add_argument('--identity', default='http://127.0.0.1:18091')
    parser.add_argument('--panel', default='http://127.0.0.1:18092')
    parser.add_argument('--ask-panel-password', action='store_true', help='Read the preserved panel password without echo after importing an older deployment')
    args = parser.parse_args()
    password = None
    if args.ask_panel_password:
        import getpass
        password = getpass.getpass('Preserved panel password: ')
    verify(args.env_file, args.site, args.identity, args.panel, password)
