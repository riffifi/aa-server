"""Capture a complete pass-sharing conversation on synthetic Python accounts."""
import base64
import json
from pathlib import Path
import sys
import tempfile
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from fastapi.testclient import TestClient
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from app.aaid import Verifier
from app.main import create_app


class NoNetwork:
    async def close(self):
        pass


def main():
    now = 1793692800
    private = Ed25519PrivateKey.from_private_bytes(bytes(range(32)))
    public = private.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    def token(person):
        encode = lambda data: base64.urlsafe_b64encode(json.dumps(data, separators=(',', ':')).encode()).rstrip(b'=')
        unsigned = encode({'alg': 'EdDSA', 'kid': 'k1'}) + b'.' + encode({'iss': 'aa-id', 'aud': 'aa', 'sub': person, 'mirea': person, 'usr': person, 'exp': now + 600})
        return (unsigned + b'.' + base64.urlsafe_b64encode(private.sign(unsigned)).rstrip(b'=')).decode()
    aliases = {}
    calls = []
    def substitute(value):
        if isinstance(value, str):
            for actual, alias in aliases.items():
                value = value.replace('$' + alias, actual)
        elif isinstance(value, dict):
            value = {key: substitute(item) for key, item in value.items()}
        return value
    def normalize(value, key=None):
        if isinstance(value, dict):
            return {name: normalize(item, name) for name, item in value.items()}
        if isinstance(value, list):
            return [normalize(item) for item in value]
        if isinstance(value, str):
            if key == 'id' and value not in aliases:
                aliases[value] = 'request' + str(1 + sum(alias.startswith('request') for alias in aliases.values()))
            if value in aliases:
                return '$' + aliases[value]
        return value
    with tempfile.TemporaryDirectory() as directory:
        app = create_app(directory, 'synthetic-admin-password', NoNetwork(), background=False,
                         aaid=Verifier(keys={'k1': public}, enforce=False))
        with TestClient(app) as client, patch('time.time', return_value=now):
            vault = app.state.service.vault
            for person in ('owner', 'guest', 'other'):
                account = vault.upsert(person, person.title(), {'cookie': 'session=' + person})
                aliases[account['id']] = person
            def call(person, method, path, body=None):
                response = client.request(method, substitute(path), json=substitute(body) if body is not None else None,
                                          headers={'Authorization': 'Bearer ' + token(person)})
                calls.append({'as': person, 'method': method, 'path': path, 'body': body,
                              'status': response.status_code, 'expected': normalize(response.json())})
            call('owner', 'GET', '/api/me/account')
            call('guest', 'GET', '/api/me/account')
            call('owner', 'PUT', '/api/me/account', {'label': 'Owner changed', 'pass_allowed': True})
            call('owner', 'GET', '/api/me/people?q=@guest')
            call('guest', 'POST', '/api/me/pass/requests', {'account_id': '$owner'})
            call('guest', 'POST', '/api/me/pass/requests', {'account_id': '$owner'})
            call('owner', 'GET', '/api/me/pass')
            call('other', 'POST', '/api/me/pass/requests/$request1/allow')
            call('owner', 'POST', '/api/me/pass/requests/$request1/allow')
            call('guest', 'GET', '/api/me/passes')
            call('owner', 'DELETE', '/api/me/pass/people/aaid/$guest')
            call('guest', 'GET', '/api/me/passes')
            call('guest', 'POST', '/api/me/pass/requests', {'account_id': '$owner'})
            call('owner', 'POST', '/api/me/pass/requests/$request2/deny')
            call('guest', 'POST', '/api/me/pass/requests', {'account_id': '$owner'})
    destination = ROOT / 'internal/httpserver/testdata/python_settings.json'
    destination.write_text(json.dumps({'now': now, 'calls': calls}, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'Captured {len(calls)} synthetic settings/sharing responses.')


if __name__ == '__main__':
    main()
