"""Verification of AA ID access tokens.

AA ID is a separate service (services/aa-id). It signs short-lived EdDSA JWTs;
this server only checks them offline against the public keys AA ID publishes,
so it never calls AA ID per request and never sees AA ID's database."""
import asyncio
import base64
import json
import re
import time
import httpx
from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

def _unb64(text):
    return base64.urlsafe_b64decode(text + '=' * (-len(text) % 4))

# Browser pages (the picker, /login and the owner's /u/{id} page) keep their own
# MIREA sign-in and page sessions; only these endpoints stay reachable without AA ID.
BROWSER_OPEN = (
    re.compile(r'^/api/logins$'),
    re.compile(r'^/api/groups/search$'),
    re.compile(r'^/api/accounts/[^/]+/page(?:/(?:login|logout|password))?$'),
)
# Owner endpoints that a browser page reaches with a page session for that account.
BROWSER_OWNER = re.compile(r'^/api/accounts/([^/]+)(?:/(?:attendance|label|end-session|pass-allowed))?$')

class Verifier:
    def __init__(self, jwks_url=None, keys=None, issuer='aa-id', audience='aa', public_url=''):
        self.jwks_url, self.keys = jwks_url, dict(keys or {})
        self.issuer, self.audience, self.public_url = issuer, audience, public_url
        self.fetched_at = 0.0
        self.lock = asyncio.Lock()
        self.http = httpx.AsyncClient(timeout=5, follow_redirects=False) if jwks_url else None

    async def _refresh(self):
        # At most one fetch a minute, so forged kids cannot hammer AA ID.
        async with self.lock:
            if self.http is None or time.time() - self.fetched_at < 60:
                return
            self.fetched_at = time.time()
            try:
                response = await self.http.get(self.jwks_url)
                keys = {key['kid']: _unb64(key['x']) for key in response.json()['keys']
                        if key.get('kty') == 'OKP' and key.get('crv') == 'Ed25519' and isinstance(key.get('kid'), str)}
            except (httpx.HTTPError, ValueError, KeyError, TypeError, AttributeError):
                return
            if keys:
                self.keys = keys

    async def verify(self, token):
        try:
            header_text, payload_text, signature_text = token.split('.')
            header = json.loads(_unb64(header_text))
            kid = header.get('kid')
            if header.get('alg') != 'EdDSA' or not isinstance(kid, str):
                return None
            if kid not in self.keys:
                await self._refresh()
            if kid not in self.keys:
                return None
            Ed25519PublicKey.from_public_bytes(self.keys[kid]).verify(_unb64(signature_text), f'{header_text}.{payload_text}'.encode())
            claims = json.loads(_unb64(payload_text))
        except (ValueError, TypeError, AttributeError, InvalidSignature):
            return None
        if not isinstance(claims, dict) or claims.get('iss') != self.issuer or claims.get('aud') != self.audience:
            return None
        if not isinstance(claims.get('exp'), int) or claims['exp'] <= time.time():
            return None
        if not isinstance(claims.get('sub'), str) or not isinstance(claims.get('mirea'), str):
            return None
        return claims

    async def close(self):
        if self.http is not None:
            await self.http.aclose()

def from_environment(environ):
    url = environ.get('AAID_JWKS_URL', '').strip()
    if not url:
        return None
    return Verifier(url, issuer=environ.get('AAID_ISSUER', 'aa-id'), audience=environ.get('AAID_AUDIENCE', 'aa'),
                    public_url=environ.get('AAID_PUBLIC_URL', '').strip())
