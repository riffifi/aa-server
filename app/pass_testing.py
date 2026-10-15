import base64
import hashlib
import hmac
import time
from datetime import datetime, timedelta, timezone

MOSCOW = timezone(timedelta(hours=3))


def daily_access(key, now=None):
    current = datetime.fromtimestamp(time.time() if now is None else now, MOSCOW)
    day = current.date().isoformat()
    midnight = datetime.combine(current.date() + timedelta(days=1), datetime.min.time(), MOSCOW)
    code = base64.b32encode(hmac.digest(key, b'pass-testing:' + day.encode(), hashlib.sha256)).decode()[:20]
    return {'password': '-'.join(code[i:i + 5] for i in range(0, 20, 5)),
            'expires_at': midnight.timestamp(), 'day': day, 'timezone': 'Europe/Moscow'}


def matches(password, access):
    candidate = password.strip().replace('-', '').upper().encode()
    expected = access['password'].replace('-', '').encode()
    return len(candidate) <= 128 and hmac.compare_digest(candidate, expected)
