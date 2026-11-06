"""Synthetic Pulse attendance/parser fixtures; no network or credentials."""
import base64
import json
import sys
from pathlib import Path
ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from app import protocol as p

def message(*parts):
    return b''.join(parts)

def run():
    stamp = lambda n: p._vfield(1, n)
    event = lambda t, code: message(p.field(2, stamp(t)), p.field(3, p.field(2, 'Кампус'.encode())), p._vfield(5, code))
    cases = []
    def case(kind, payload):
        body = p.frame(payload) + p.frame(b'grpc-status: 0\r\n', 128)
        function = {'acs': p.acs_events, 'logs': p.visiting_logs, 'lessons': p.day_lessons, 'status': p.lesson_attendance}[kind]
        cases.append({'kind': kind, 'body': base64.b64encode(body).decode(), 'expected': function(body, {})})
    case('acs', message(*(p.field(1, event(t, code)) for t, code in [(1793690000, 1), (1793691000, 3), (1793692000, 2)])))
    journal = message(p.field(1, b'journal'), p.field(2, 'Группа'.encode()), p.field(6, message(p.field(3, stamp(1700000000)), p.field(4, stamp(1900000000)))))
    case('logs', p.field(2, p.field(2, journal)))
    lesson = message(p.field(1, b'lesson'), p.field(2, stamp(1793690000)), p.field(3, stamp(1793695400)), p.field(4, p.field(2, 'Физика'.encode())))
    case('lessons', p.field(2, p.field(3, lesson)))
    students = [p.field(1, message(p.field(7, p.field(1, ('enum|' + mark).encode())), p.field(9, uid.encode()))) for uid, mark in [('ann', 'Present'), ('bob', 'Absent'), ('cat', 'ExcusedAbsence')]]
    case('status', message(*students))
    for kind in ['acs', 'logs', 'lessons', 'status']:
        case(kind, b'')
    (ROOT / 'internal/pulse/testdata').mkdir(exist_ok=True)
    (ROOT / 'internal/pulse/testdata/python_journal.json').write_text(json.dumps(cases, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'Captured {len(cases)} synthetic journal cases')
if __name__ == '__main__':
    run()
