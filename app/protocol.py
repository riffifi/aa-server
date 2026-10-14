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
    'schedules': '/rtu.schedule.api.PersonalScheduleService/GetSubscribedSchedules',
    'acs-stats': '/rtu_tc.rtu_attend.humanpass.HumanPassService/GetHumanAcsAttendanceStats',
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

def _read_varint(payload, offset):
    value = 0
    for shift in range(0, 70, 7):
        if offset >= len(payload):
            raise ValueError('Truncated varint')
        byte = payload[offset]
        offset += 1
        value |= (byte & 127) << shift
        if not byte & 128:
            return value, offset
    raise ValueError('Invalid varint')

def _as_text(data):
    try:
        text = data.decode('utf-8')
    except UnicodeError:
        return None
    if any(ord(c) < 32 and c not in '\t\n\r' for c in text):
        return None
    return text

def decode_tree(payload, depth=0):
    """Best-effort, tolerant decode of a protobuf message into a JSON-friendly
    tree for diagnostics. Length-delimited fields become a nested object when they
    parse cleanly as a message, otherwise a UTF-8 string, otherwise a hex preview.
    Never raises: it returns whatever it could decode before hitting bad bytes."""
    out = {}
    offset = 0
    try:
        while offset < len(payload):
            tag, offset = _read_varint(payload, offset)
            number, wire = tag >> 3, tag & 7
            if number == 0:
                break
            if wire == 0:
                value, offset = _read_varint(payload, offset)
            elif wire in (1, 5):
                size = 8 if wire == 1 else 4
                if offset + size > len(payload):
                    break
                value = int.from_bytes(payload[offset:offset + size], 'little')
                offset += size
            elif wire == 2:
                length, offset = _read_varint(payload, offset)
                if length < 0 or offset + length > len(payload):
                    break
                data = payload[offset:offset + length]
                offset += length
                nested = decode_tree(data, depth + 1) if depth < 6 and data else {}
                text = _as_text(data)
                if nested and text is None:
                    value = nested
                elif text is not None:
                    value = text
                elif nested:
                    value = nested
                else:
                    value = {'_hex': data[:64].hex(), '_len': len(data)}
            else:
                break
            key = str(number)
            if key in out:
                if not isinstance(out[key], list):
                    out[key] = [out[key]]
                out[key].append(value)
            else:
                out[key] = value
    except (ValueError, IndexError):
        pass
    return out

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

def _vfield(number, value):
    return varint(number << 3) + varint(value)

def _timestamp(seconds, nanos=0):
    out = _vfield(1, seconds)
    if nanos:
        out += _vfield(2, nanos)
    return out

def acs_stats_request(uid, start, end):
    """Build the GetHumanAcsAttendanceStats request: the human id, a [start, end]
    window (protobuf Timestamps) and the flag the app sends (3=1)."""
    window = field(1, _timestamp(int(start))) + field(2, _timestamp(int(end), 999000000))
    message = field(1, uid.encode()) + field(2, window) + _vfield(3, 1)
    return frame(message)

def acs_sessions(body, headers):
    """Parse GetHumanAcsAttendanceStats into attendance sessions. Each session (field 3,
    repeated) carries the entry time in field 1 and the exit time in field 2 (verified
    against real data: 09:02 → 17:59). A session with no exit means still inside.
    Returns (sessions_sorted_by_entry, inside_now)."""
    try:
        message, status = grpc_message(body, headers)
    except (ValueError, UnicodeError):
        return [], False
    if status != 0 or message is None:
        return [], False
    raw = decode_tree(message).get('3')
    if isinstance(raw, dict):
        raw = [raw]
    if not isinstance(raw, list):
        return [], False
    sessions = []
    for item in raw:
        if not isinstance(item, dict):
            continue
        enter = item.get('1')
        enter = enter.get('1') if isinstance(enter, dict) else None
        leave = item.get('2')
        leave = leave.get('1') if isinstance(leave, dict) else None
        if not isinstance(enter, int):
            continue
        sessions.append({'enter': enter, 'exit': leave if isinstance(leave, int) else None})
    sessions.sort(key=lambda s: s['enter'])
    inside = bool(sessions) and sessions[-1]['exit'] is None
    return sessions, inside

def subscribed_group(body, headers):
    """Parse GetSubscribedSchedules into (group_id, group_name). The response lists
    the schedules the account is subscribed to; each entry's target carries a
    scheduleTarget (1 = group, 2 = teacher, 3 = auditorium) and a numeric id in the
    same space as schedule-of.mirea.ru. We return the group entry, preferring the
    one flagged default (field 4 == 1). Returns (None, None) when there is none."""
    try:
        message, status = grpc_message(body, headers)
    except (ValueError, UnicodeError):
        return None, None
    if status != 0 or message is None:
        return None, None
    entries = decode_tree(message).get('1')
    if isinstance(entries, dict):
        entries = [entries]
    if not isinstance(entries, list):
        return None, None
    best = None
    for item in entries:
        if not isinstance(item, dict):
            continue
        target = item.get('1')
        if not isinstance(target, dict) or target.get('1') != 1:
            continue
        group_id = target.get('2')
        if not isinstance(group_id, int) or group_id <= 0:
            continue
        name = item.get('2') if isinstance(item.get('2'), str) else None
        if item.get('4') == 1:
            return group_id, name
        if best is None:
            best = (group_id, name)
    return best if best else (None, None)

def token_expiry(token):
    try:
        payload = token.split('.')[1]
        expiry = json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))['exp']
        if type(expiry) is not int or expiry <= 0:
            raise ValueError('Invalid expiry')
        return expiry
    except (ValueError, KeyError, IndexError, TypeError):
        raise ValueError('Invalid pass token') from None
