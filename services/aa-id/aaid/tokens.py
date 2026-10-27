"""Ed25519-signed JWT access tokens (alg EdDSA) and the service's signing key.

Other services verify these tokens offline with the public key published at
/.well-known/jwks.json, so AA ID is never on the hot path of their requests."""
import base64
import hashlib
import json
import os
import time
from pathlib import Path
from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

def b64(data):
    return base64.urlsafe_b64encode(data).rstrip(b'=').decode()

def unb64(text):
    return base64.urlsafe_b64decode(text + '=' * (-len(text) % 4))

class Signer:
    def __init__(self, directory):
        key_file = Path(directory) / 'signing.key'
        try:
            descriptor = os.open(key_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, 'wb') as stream:
                stream.write(Ed25519PrivateKey.generate().private_bytes(
                    serialization.Encoding.Raw, serialization.PrivateFormat.Raw, serialization.NoEncryption()))
        except FileExistsError:
            pass
        self.key = Ed25519PrivateKey.from_private_bytes(key_file.read_bytes())
        self.public = self.key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
        self.kid = hashlib.sha256(self.public).hexdigest()[:16]

    def jwks(self):
        return {'keys': [{'kty': 'OKP', 'crv': 'Ed25519', 'x': b64(self.public), 'kid': self.kid, 'alg': 'EdDSA', 'use': 'sig'}]}

    def sign(self, claims):
        header = b64(json.dumps({'alg': 'EdDSA', 'typ': 'JWT', 'kid': self.kid}, separators=(',', ':')).encode())
        payload = b64(json.dumps(claims, separators=(',', ':')).encode())
        signature = self.key.sign(f'{header}.{payload}'.encode())
        return f'{header}.{payload}.{b64(signature)}'

def verify(token, keys, issuer, audience, now=None):
    """Return the claims of a valid token, or None. `keys` maps kid -> raw public key."""
    try:
        header_text, payload_text, signature_text = token.split('.')
        header = json.loads(unb64(header_text))
        if header.get('alg') != 'EdDSA' or header.get('kid') not in keys:
            return None
        Ed25519PublicKey.from_public_bytes(keys[header['kid']]).verify(unb64(signature_text), f'{header_text}.{payload_text}'.encode())
        claims = json.loads(unb64(payload_text))
    except (ValueError, TypeError, AttributeError, InvalidSignature):
        return None
    now = time.time() if now is None else now
    if not isinstance(claims, dict) or claims.get('iss') != issuer or claims.get('aud') != audience:
        return None
    if not isinstance(claims.get('exp'), int) or claims['exp'] <= now or not isinstance(claims.get('sub'), str):
        return None
    return claims
