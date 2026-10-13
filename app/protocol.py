"""Small, bounded gRPC-Web codec for the Pulse calls this service permits."""
import struct
import json
import base64
from urllib.parse import urlparse

HOST = 'https://pulse.mirea.ru'
LOGIN_URL = HOST + '/api/auth/login?redirectUri=https%3A%2F%2Fpulse.mirea.ru%2Fservices&rememberMe=True'
METHODS = {
    'profile': '/rtu_tc.rtu_attend.app.UserService/GetMeInfo',
    'attendance': '/rtu_tc.attendance.api.AttendanceService/SelfApproveAttendanceThroughQRCode',
    'pass-token': '/rtu.pulse_app.LongTimeTokenService/GetAccessTokenForDigitalPass',
    'pass-code': '/rtu_tc.rtu_attend.humanpass.HumanPassService/SendVerificationCode',
    'pass-issue': '/rtu_tc.rtu_attend.humanpass.HumanPassService/GetDigitalPass',
}
PROFILE_PATH = METHODS['profile']

def varint(value):
    result = bytearray()
    while value > 127:
        result.append((value & 127) | 128)
        value >>= 7
    result.append(value)
    return bytes(result)

def field(number, value):
    return varint((number << 3) | 2) + varint(len(value)) + value

def frame(payload, flag=0):
    return bytes([flag]) + struct.pack('>I', len(payload)) + payload

def profile_request():
    return frame(field(1, HOST.encode()) + b'\x18\x01')

def fields(payload):
    offset = 0
    result = {}
    def read():
        nonlocal offset
        value = 0
        for shift in range(0, 70, 7):
            if offset >= len(payload):
                raise ValueError('Truncated protobuf')
            byte = payload[offset]
            offset += 1
            value |= (byte & 127) << shift
            if not byte & 128:
                return value
        raise ValueError('Invalid varint')
    while offset < len(payload):
        tag = read()
        if tag >> 3 == 0:
            raise ValueError('Invalid protobuf tag')
        wire = tag & 7
        if wire == 0:
            result[tag >> 3] = read()
            continue
        size = read() if wire == 2 else {1: 8, 5: 4}.get(wire)
        if size is None or size < 0 or size > len(payload) - offset:
            raise ValueError('Invalid protobuf field')
        if wire == 2:
            result[tag >> 3] = payload[offset:offset + size]
        offset += size
    return result

def grpc_message(body, headers):
    if not headers.get('content-type', '').lower().startswith('application/grpc-web'):
        raise ValueError('Not a gRPC-Web response')
    offset = 0
    message = None
    status = headers.get('grpc-status')
    while offset < len(body):
        if len(body) - offset < 5:
            raise ValueError('Truncated frame')
        flag, size = body[offset], struct.unpack_from('>I', body, offset + 1)[0]
        offset += 5
        if size > len(body) - offset:
            raise ValueError('Truncated body')
        payload = body[offset:offset + size]
        offset += size
        if flag == 0:
            if message is not None:
                raise ValueError('Multiple unary messages')
            message = payload
        elif flag == 128:
            for line in payload.decode('ascii').split('\r\n'):
                key, sep, value = line.partition(':')
                if sep and key.lower() == 'grpc-status':
                    if status is not None and status != value.strip():
                        raise ValueError('Conflicting status')
                    status = value.strip()
        else:
            raise ValueError('Unsupported frame')
    if status is None or not status.isdigit():
        raise ValueError('Missing status')
    return message, int(status)

def profile_identity(body, headers):
    message, status = grpc_message(body, headers)
    if status == 16:
        return 'expired', None
    if status == 7:
        return 'forbidden', None
    if status != 0 or message is None:
        return 'unavailable', None
    outer = fields(message)
    if 1 in outer and 2 in outer:
        raise ValueError('Conflicting identity')
    if 2 in outer:
        return 'expired', None
    authorized = fields(outer.get(1, b''))
    user = fields(authorized.get(1, b''))
    uid = user.get(1, b'').decode('utf-8')
    if not uid or len(uid) > 256:
        raise ValueError('Missing identity')
    return 'valid', uid

def authentication_state(status, headers, body):
    if status == 401:
        return 'expired', None
    if status == 403:
        return 'forbidden', None
    if status in (301, 302, 303, 307, 308):
        redirect = urlparse(headers.get('location', ''))
        return ('expired' if '/auth/login' in redirect.path or redirect.hostname == 'sso.mirea.ru' else 'unavailable'), None
    if status != 200:
        return 'unavailable', None
    try:
        return profile_identity(body, headers)
    except (ValueError, UnicodeError, TypeError):
        return 'invalidResponse', None

def token_expiry(token):
    try:
        payload = token.split('.')[1]
        expiry = json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))['exp']
        if type(expiry) is not int or expiry <= 0:
            raise ValueError('Invalid expiry')
        return expiry
    except (ValueError, KeyError, IndexError, TypeError):
        raise ValueError('Invalid pass token') from None
