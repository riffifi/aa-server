"""Generate synthetic compatibility fixtures and inventory; never reads production data."""
import ast
import base64
import hashlib
import json
from pathlib import Path
import random
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from app import protocol, pass_testing
from app.vault import password_hash
from cryptography.fernet import Fernet
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


def b64(data):
    return base64.b64encode(data).decode('ascii')


def main():
    now = 1793692800
    key = base64.urlsafe_b64encode(bytes(range(32)))
    plaintext = json.dumps({'cookie': 'session=synthetic', 'label': 'Проверка'}, ensure_ascii=False).encode()
    seed = bytes(range(32, 64))
    private = Ed25519PrivateKey.from_private_bytes(seed)
    public = private.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    kid = hashlib.sha256(public).hexdigest()[:16]
    claims = {'iss': 'aa-id', 'aud': 'aa', 'sub': 'synthetic-aaid', 'mirea': 'synthetic-user', 'exp': now + 600}
    encode = lambda value: base64.urlsafe_b64encode(json.dumps(value, separators=(',', ':')).encode()).rstrip(b'=')
    unsigned = encode({'alg': 'EdDSA', 'typ': 'JWT', 'kid': kid}) + b'.' + encode(claims)
    token = (unsigned + b'.' + base64.urlsafe_b64encode(private.sign(unsigned)).rstrip(b'=')).decode()
    headers = {'content-type': 'application/grpc-web+proto'}
    profile = protocol.field(1, protocol.field(1, protocol.field(1, b'synthetic-user')))
    success = protocol.frame(profile) + protocol.frame(b'grpc-status: 0\r\n', 128)
    samples = [('valid', 200, success, headers), ('expired-http', 401, b'', {}),
               ('forbidden-http', 403, b'', {}), ('invalid', 200, b'bad', headers),
               ('expired-grpc', 200, protocol.frame(b'grpc-status: 16\r\n', 128), headers),
               ('duplicate', 200, success + protocol.frame(b''), headers),
               ('conflicting-status', 200, success, headers | {'grpc-status': '7'}),
               ('sso-redirect', 302, b'', {'location': 'https://sso.mirea.ru/login'}),
               ('other-redirect', 302, b'', {'location': 'https://pulse.mirea.ru/other'})]
    wire = []
    for name, status, body, hdr in samples:
        state, uid = protocol.authentication_state(status, hdr, body)
        try:
            message, grpc_status = protocol.grpc_message(body, hdr)
            error = False
        except (ValueError, UnicodeError):
            message, grpc_status, error = None, 0, True
        wire.append(dict(name=name, status=status, body=b64(body), headers=hdr, state=state, uid=uid,
                         message=b64(message) if message is not None else None, grpc_status=grpc_status, error=error))
    rng = random.Random(42)
    trees = [profile, protocol.field(1, 'Группа'.encode()) + protocol.varint(24) + protocol.varint(42),
             protocol.field(1, b'a') + protocol.field(1, b'b')]
    trees.extend(rng.randbytes(rng.randrange(0, 80)) for _ in range(100))
    document = {
        'now': now,
        'fernet': {'key': key.decode(), 'plaintext': b64(plaintext), 'token': Fernet(key).encrypt_at_time(plaintext, now).decode()},
        'password': {'password': 'synthetic-test-password', 'salt': '01' * 16,
                     'hash': password_hash('synthetic-test-password', '01' * 16)},
        'daily': [{'key': b64(b'synthetic-key'), 'now': moment, 'expected': pass_testing.daily_access(b'synthetic-key', moment)}
                  for moment in (now, 1792097999, 1792098000)],
        'jwt': {'seed': b64(seed), 'public': b64(public), 'kid': kid, 'token': token, 'claims': claims},
        'wire': wire,
        'trees': [{'payload': b64(payload), 'expected': protocol.decode_tree(payload)} for payload in trees],
        'requests': {'profile': b64(protocol.profile_request()),
                     'acs': b64(protocol.acs_timeline_request('synthetic-user', 1000, 2000)),
                     'attendance': b64(protocol.lesson_attendance_request('lesson', 'log'))},
    }
    destination = ROOT / 'internal' / 'contracttest' / 'testdata'
    destination.mkdir(parents=True, exist_ok=True)
    (destination / 'python.json').write_text(json.dumps(document, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    routes = []
    for source in ('app/main.py', 'app/site.py', 'services/aa-id/aaid/main.py'):
        tree = ast.parse((ROOT / source).read_text(encoding='utf-8'))
        for node in ast.walk(tree):
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            for decorator in node.decorator_list:
                if (isinstance(decorator, ast.Call) and isinstance(decorator.func, ast.Attribute)
                        and decorator.func.attr in ('get', 'post', 'put', 'delete', 'patch') and decorator.args
                        and isinstance(decorator.args[0], ast.Constant)):
                    routes.append({'service': 'aaid' if source.startswith('services/') else 'sessions',
                                   'method': decorator.func.attr.upper(), 'path': decorator.args[0].value,
                                   'source': source, 'handler': node.name, 'native_go': False})
    (destination / 'routes.json').write_text(json.dumps(sorted(routes, key=lambda r: (r['service'], r['path'], r['method'])), indent=2) + '\n')
    print(f'Wrote synthetic fixtures and {len(routes)} route contracts; no production data used.')


if __name__ == '__main__':
    main()
