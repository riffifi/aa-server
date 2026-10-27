"""Minimal MIREA Pulse client: proves who owns a Pulse session.

AA ID never stores or logs the session cookie. It asks Pulse who the cookie
belongs to (GetMeInfo) and which group the person studies in
(GetSubscribedSchedules), then forgets the cookie."""
import re
import struct
import httpx

HOST = 'https://pulse.mirea.ru'
PROFILE = '/rtu_tc.rtu_attend.app.UserService/GetMeInfo'
SCHEDULES = '/rtu.schedule.api.PersonalScheduleService/GetSubscribedSchedules'
MAX_RESPONSE = 1024 * 1024
COOKIE_NAME = re.compile(r"^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

def validate_cookie(header):
    if not isinstance(header, str) or not header or len(header) > 65536:
        raise ValueError('Invalid session cookie')
    seen = set()
    for item in header.split(';'):
        name, sep, value = item.strip().partition('=')
        if not sep or name in seen or not COOKIE_NAME.fullmatch(name) or not value or any(ord(c) < 33 or ord(c) > 126 or c in '\",;\\' for c in value):
            raise ValueError('Invalid session cookie')
        seen.add(name)
    return header

def varint(value):
    result = bytearray()
    while value > 127:
        result.append((value & 127) | 128)
        value >>= 7
    result.append(value)
    return bytes(result)

def frame(payload, flag=0):
    return bytes([flag]) + struct.pack('>I', len(payload)) + payload

def profile_request():
    origin = HOST.encode()
    return frame(varint(10) + varint(len(origin)) + origin + b'\x18\x01')

def fields(payload):
    """Decode one protobuf message into {field: value}; repeated fields become lists."""
    offset, result = 0, {}
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
        number, wire = tag >> 3, tag & 7
        if number == 0:
            raise ValueError('Invalid protobuf tag')
        if wire == 0:
            value = read()
        else:
            size = read() if wire == 2 else {1: 8, 5: 4}.get(wire)
            if size is None or size > len(payload) - offset:
                raise ValueError('Invalid protobuf field')
            value = payload[offset:offset + size] if wire == 2 else None
            offset += size
            if value is None:
                continue
        if number in result:
            if not isinstance(result[number], list):
                result[number] = [result[number]]
            result[number].append(value)
        else:
            result[number] = value
    return result

def grpc_message(body, headers):
    if not headers.get('content-type', '').lower().startswith('application/grpc-web'):
        raise ValueError('Not a gRPC-Web response')
    offset, message, status = 0, None, headers.get('grpc-status')
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
                    status = value.strip()
        else:
            raise ValueError('Unsupported frame')
    if status is None or not status.isdigit():
        raise ValueError('Missing status')
    return message, int(status)

def profile_uid(status, headers, body):
    """Return the Pulse person id behind a GetMeInfo reply, or None if the session is not valid."""
    if status != 200:
        return None
    message, grpc_status = grpc_message(body, headers)
    if grpc_status != 0 or message is None:
        return None
    outer = fields(message)
    if 2 in outer or not isinstance(outer.get(1), bytes):
        return None
    user = fields(fields(outer[1]).get(1, b''))
    uid = user.get(1)
    if not isinstance(uid, bytes):
        return None
    uid = uid.decode('utf-8')
    return uid if 0 < len(uid) <= 256 else None

def subscribed_group(headers, body):
    """(group_id, group_name) of the person's default group schedule, or (None, None)."""
    try:
        message, status = grpc_message(body, headers)
        if status != 0 or message is None:
            return None, None
        entries = fields(message).get(1, [])
        best = (None, None)
        for item in entries if isinstance(entries, list) else [entries]:
            entry = fields(item)
            target = fields(entry.get(1, b'')) if isinstance(entry.get(1), bytes) else {}
            if target.get(1) != 1 or not isinstance(target.get(2), int) or target[2] <= 0:
                continue
            name = entry.get(2).decode('utf-8') if isinstance(entry.get(2), bytes) else None
            if entry.get(4) == 1:
                return target[2], name
            if best[0] is None:
                best = (target[2], name)
        return best
    except (ValueError, UnicodeError):
        return None, None

class PulseIdentity:
    def __init__(self):
        self.http = httpx.AsyncClient(timeout=15, follow_redirects=False)

    async def _call(self, cookie, path, body):
        headers = {'content-type': 'application/grpc-web+proto', 'accept': 'application/grpc-web+proto',
                   'x-grpc-web': '1', 'x-requested-with': 'XMLHttpRequest', 'origin': HOST,
                   'referer': HOST + '/services', 'cookie': validate_cookie(cookie)}
        async with self.http.stream('POST', HOST + path, content=body, headers=headers) as response:
            data = bytearray()
            async for chunk in response.aiter_bytes():
                if len(data) + len(chunk) > MAX_RESPONSE:
                    raise ValueError('Upstream response exceeds limit')
                data.extend(chunk)
            return response.status_code, dict(response.headers), bytes(data)

    async def whoami(self, cookie):
        """Return (pulse_uid, group_id, group_name). pulse_uid is None for an invalid session.
        Raises OSError/httpx.HTTPError when Pulse cannot be reached."""
        status, headers, body = await self._call(cookie, PROFILE, profile_request())
        uid = profile_uid(status, headers, body)
        if uid is None:
            return None, None, None
        try:
            status, headers, body = await self._call(cookie, SCHEDULES, frame(b''))
            group_id, group_name = subscribed_group(headers, body) if status == 200 else (None, None)
        except (httpx.HTTPError, OSError, ValueError):
            group_id, group_name = None, None
        return uid, group_id, group_name

    async def close(self):
        await self.http.aclose()
