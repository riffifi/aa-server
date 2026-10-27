"""Small, bounded gRPC-Web codec for the Pulse calls this service permits."""
import struct
import json
import base64
from urllib.parse import parse_qs, urlparse

HOST = 'https://pulse.mirea.ru'
LOGIN_URL = HOST + '/api/auth/login?redirectUri=https%3A%2F%2Fpulse.mirea.ru%2Fservices&rememberMe=True'
METHODS = {
    'profile': '/rtu_tc.rtu_attend.app.UserService/GetMeInfo',
    'attendance': '/rtu_tc.attendance.api.AttendanceService/SelfApproveAttendanceThroughQRCode',
    'pass-token': '/rtu.pulse_app.LongTimeTokenService/GetAccessTokenForDigitalPass',
    'pass-code': '/rtu_tc.rtu_attend.humanpass.HumanPassService/SendVerificationCode',
    'pass-issue': '/rtu_tc.rtu_attend.humanpass.HumanPassService/GetDigitalPass',
    'schedules': '/rtu.schedule.api.PersonalScheduleService/GetSubscribedSchedules',
    'acs-timeline': '/rtu_tc.rtu_attend.humanpass.HumanPassService/GetHumanAcsTimeline',
    'visiting-logs': '/rtu_tc.attendance.api.VisitingLogService/GetAvailableVisitingLogsOfStudent',
    'day-lessons': '/rtu_tc.attendance.api.LessonService/GetAvailableLessonsOfVisitingLogs',
    'lesson-attendance': '/rtu_tc.attendance.api.AttendanceService/GetAttendanceForLesson',
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

def acs_timeline_request(uid, start, end):
    """Build the GetHumanAcsTimeline request: the human id, a [start, end] window
    (protobuf Timestamps) and the two flags the app sends (3=1, 4=2)."""
    window = field(1, _timestamp(int(start))) + field(2, _timestamp(int(end), 999000000))
    message = field(1, uid.encode()) + field(2, window) + _vfield(3, 1) + _vfield(4, 2)
    return frame(message)

# ACS event direction, verified against real passes: at the inner turnstile code 1 is
# an entry (вход) and code 2 is an exit (выход). The outer checkpoint (ЦКПП) emits
# code 3 in both directions, so it is not directional and is ignored.
ACS_DIRECTION = {1: 'in', 2: 'out'}

def _collect_acs_events(node, out):
    if isinstance(node, dict):
        stamp = node.get('2')
        when = stamp.get('1') if isinstance(stamp, dict) else None
        code = node.get('5')
        if isinstance(when, int) and isinstance(code, int) and isinstance(node.get('3'), dict):
            zone = node['3'].get('2')
            out.append({'time': when, 'code': code, 'zone': zone if isinstance(zone, str) else None})
        for value in node.values():
            _collect_acs_events(value, out)
    elif isinstance(node, list):
        for value in node:
            _collect_acs_events(value, out)

def acs_events(body, headers):
    """Parse GetHumanAcsTimeline into directional turnstile events. The response nests
    events in a few shapes, so they are collected recursively (an event is any submessage
    with a Timestamp in field 2, a zone in field 3 and a direction code in field 5).
    Only directional codes (1=entry, 2=exit) are kept. Returns (events_sorted, inside)."""
    try:
        message, status = grpc_message(body, headers)
    except (ValueError, UnicodeError):
        return [], False
    if status != 0 or message is None:
        return [], False
    raw = []
    _collect_acs_events(decode_tree(message), raw)
    events = []
    for event in raw:
        direction = ACS_DIRECTION.get(event['code'])
        if direction is None:
            continue
        events.append({'direction': direction, 'time': event['time'], 'zone': event['zone']})
    events.sort(key=lambda e: e['time'])
    inside = bool(events) and events[-1]['direction'] == 'in'
    return events, inside

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

def attendance_token(value):
    """The lesson token from a Pulse attendance QR link, as the app reads it."""
    url = urlparse(value.strip())
    if url.scheme != 'https' or url.hostname not in ('pulse.mirea.ru', 'attendance-app.mirea.ru') or url.username or url.password:
        raise ValueError('Not a Pulse attendance link')
    tokens = parse_qs(url.query).get('token', [])
    if len(tokens) != 1 or not tokens[0] or len(tokens[0]) > 4096:
        raise ValueError('Missing lesson token')
    return tokens[0]

APPROVAL_REASONS = {1: 'Неверный или просроченный QR-код', 2: 'Ожидается подтверждение',
                    3: 'Аккаунт не записан на занятие', 4: 'Журнал занятия в архиве',
                    5: 'Присутствие в кампусе не подтверждено', 6: 'Указано уважительное отсутствие',
                    7: 'Указано нарушение распорядка'}

def approval(status, headers, body):
    """Parse SelfApproveAttendanceThroughQRCode into (state, message). The state is
    approved, waiting, rejected, expired (the MIREA session) or error."""
    if status == 401 or status in (301, 302, 303, 307, 308):
        return 'expired', 'Сессия МИРЭА истекла'
    if status != 200:
        return 'error', f'Пульс вернул HTTP {status}'
    try:
        message, grpc_status = grpc_message(body, headers)
    except (ValueError, UnicodeError):
        return 'error', 'Некорректный ответ Пульса'
    if grpc_status == 16:
        return 'expired', 'Сессия МИРЭА истекла'
    if grpc_status != 0 or message is None:
        return 'error', f'Пульс отклонил запрос (gRPC {grpc_status})'
    try:
        outer = fields(message)
        if 2 in outer:
            return 'approved', 'Присутствие подтверждено'
        if 1 in outer:
            reason = fields(outer[1]).get(1, 0) if isinstance(outer[1], bytes) else 0
            reason = reason if isinstance(reason, int) else 0
            return ('waiting' if reason == 2 else 'rejected'), APPROVAL_REASONS.get(reason, 'Пульс не подтвердил присутствие')
    except (ValueError, TypeError):
        pass
    return 'error', 'Неизвестный ответ Пульса'

# Lesson attendance in Pulse's group journal ("visiting log"), as the Pulse web app
# reads it: the student's journals, the journal lessons on a day, and every student's
# status at one lesson. Field numbers come from recorded Pulse traffic.
ATTENDANCE = {'Present': 'present', 'Absent': 'absent', 'ExcusedAbsence': 'excused'}

def _many(value):
    return value if isinstance(value, list) else [] if value is None else [value]

def _message(body, headers):
    try:
        message, status = grpc_message(body, headers)
    except (ValueError, UnicodeError):
        return None
    return decode_tree(message) if status == 0 and message else ({} if status == 0 else None)

def day_lessons_request(day):
    return frame(field(2, _vfield(1, day.year) + _vfield(2, day.month) + _vfield(3, day.day)))

def lesson_attendance_request(lesson_id, log_id):
    return frame(field(1, lesson_id.encode()) + field(2, log_id.encode()))

def visiting_logs(body, headers):
    """The student's group journals: [{'id', 'group', 'start', 'end'}] (Unix times)."""
    tree = _message(body, headers)
    result = []
    for item in _many((tree or {}).get('2')):
        journal = item.get('2') if isinstance(item, dict) else None
        if not isinstance(journal, dict) or not isinstance(journal.get('1'), str):
            continue
        semester = journal.get('6') if isinstance(journal.get('6'), dict) else {}
        stamp = lambda key: semester.get(key, {}).get('1') if isinstance(semester.get(key), dict) else None
        result.append({'id': journal['1'], 'group': journal.get('2') if isinstance(journal.get('2'), str) else None,
                       'start': stamp('3'), 'end': stamp('4')})
    return None if tree is None else result

def day_lessons(body, headers):
    """Journal lessons on the requested day: [{'id', 'start', 'end', 'subject'}]."""
    tree = _message(body, headers)
    result = []
    for item in _many((tree or {}).get('2')):
        lesson = item.get('3') if isinstance(item, dict) else None
        if not isinstance(lesson, dict) or not isinstance(lesson.get('1'), str):
            continue
        start, end = lesson.get('2'), lesson.get('3')
        discipline = lesson.get('4') if isinstance(lesson.get('4'), dict) else {}
        if isinstance(start, dict) and isinstance(end, dict) and isinstance(start.get('1'), int) and isinstance(end.get('1'), int):
            result.append({'id': lesson['1'], 'start': start['1'], 'end': end['1'],
                           'subject': discipline.get('2') if isinstance(discipline.get('2'), str) else ''})
    return None if tree is None else result

def lesson_attendance(body, headers):
    """Every student's status at a lesson: {human id: 'present' | 'absent' | 'excused'}."""
    tree = _message(body, headers)
    result = {}
    for student in _many((tree or {}).get('1')):
        if not isinstance(student, dict) or not isinstance(student.get('9'), str):
            continue
        mark = student.get('7')
        label = mark.get('1') if isinstance(mark, dict) else None
        if isinstance(label, str) and '|' in label:
            word = label.rsplit('|', 1)[1]
            result[student['9']] = ATTENDANCE.get(word, word.lower()[:32])
    return None if tree is None else result
