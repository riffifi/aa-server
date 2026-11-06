"""Capture HTTP contracts using synthetic data and a transport that never uses the network."""
import json
from pathlib import Path
import sys
import tempfile
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from fastapi.testclient import TestClient
from app.main import create_app


class NoNetwork:
    async def close(self):
        pass


def main():
    cases = [('GET', '/health', None), ('GET', '/api/groups', None),
             ('GET', '/api/accounts', None), ('GET', '/api/events?wait=0', None),
             ('GET', '/api/accounts/missing', None)]
    for value in ('0', '-1', 'abc', '1.0', '9223372036854775808'):
        cases.append(('GET', '/api/accounts?group_id=' + value, None))
    for query in ('after=-1&wait=0', 'after=abc&wait=0', 'wait=26', 'wait=-1', 'wait=abc'):
        cases.append(('GET', '/api/events?' + query, None))
    for body in ('', '{bad', 'null', '{}', '{"password":1}', '{"password":""}'):
        cases.append(('POST', '/panel/api/login', body))
    for body in ('{}', '{"label":1}', '{"label":""}', '{"label":"New"}',
                 '{"label":"New","expected_version":-1}', '{"label":"New","expected_version":"bad"}'):
        cases.append(('PUT', '/api/accounts/missing/label', body))
    for body in ('{}', '{"allowed":null}', '{"allowed":"bad"}', '{"allowed":true}', '{"allowed":"yes"}'):
        cases.append(('PUT', '/api/accounts/missing/pass-allowed', body))
    result = []
    with tempfile.TemporaryDirectory() as directory:
        app = create_app(directory, 'synthetic-admin-password', NoNetwork(), background=False)
        with TestClient(app) as client, patch('time.time', return_value=1793692800):
            for method, path, body in cases:
                response = client.request(method, path, content=body, headers={'content-type': 'application/json'} if body is not None else {})
                result.append({'method': method, 'path': path, 'body': body or '',
                               'status': response.status_code, 'expected': response.json()})
    destination = ROOT / 'internal' / 'httpserver' / 'testdata'
    destination.mkdir(parents=True, exist_ok=True)
    (destination / 'python_http.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'Wrote {len(result)} synthetic HTTP contracts; no production network used.')


if __name__ == '__main__':
    main()
