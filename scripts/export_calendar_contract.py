"""Capture synthetic recurrence and class-matching results from the Python parser."""
import json
from datetime import datetime
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from app import schedule


def event(start, end, subject, extra='', room='А-101'):
    return f'BEGIN:VEVENT\r\nDTSTART;TZID=Europe/Moscow:{start}\r\nDTEND;TZID=Europe/Moscow:{end}\r\nSUMMARY:{subject}\r\nLOCATION:{room}\r\nX-META-TEACHER:Иванов\r\n{extra}END:VEVENT\r\n'


def main():
    first = datetime(2026, 11, 2)
    sources = [
        'BEGIN:VCALENDAR\r\n' + event('20260901T090000', '20260901T103000', 'Математика',
            'RRULE:FREQ=WEEKLY;BYDAY=TU,TH;UNTIL=20261130T235959Z\r\nRDATE:20261104T090000\r\nEXDATE:20261105T090000\r\n')
        + event('20261103T090000', '20261103T103000', 'Математика', room='Б-202')
        + event('20261103T090000', '20261103T103000', 'Физика')
        + event('20261103T120000', '20261103T133000', 'Пропустить', 'TRANSP:TRANSPARENT\r\n') + 'END:VCALENDAR\r\n',
        'BEGIN:VCALENDAR\r\n' + event('20261102T2300Z', '20261103T0100Z', r'Ночная\, пара',
            'RDATE:20261104T230000,20261106T230000\r\nEXDATE:20261104T230000\r\nX-META-TEACHER:Петров\r\n') + 'END:VCALENDAR\r\n',
        'BEGIN:VCALENDAR\r\n' + event('20261102T090000', '20261102T100000', 'Не расширять',
            'RRULE:FREQ=INVALID\r\nRDATE:20261103T090000\r\n') + 'END:VCALENDAR\r\n',
    ]
    cases = []
    for source in sources:
        days = schedule.lessons_between(source, first.date(), 7)
        cases.append({'source': source, 'first': first.date().isoformat(), 'days': 7,
                      'expected': {day.isoformat(): [{**lesson, 'start': lesson['start'].isoformat(), 'end': lesson['end'].isoformat()}
                                                     for lesson in lessons] for day, lessons in days.items()}})
    lesson = {'start': first.replace(hour=9), 'end': first.replace(hour=10), 'subject': 'Математика', 'teacher': 'Иванов', 'location': 'А\u00a0 101'}
    pairs = []
    for other in (lesson | {'location': 'а 101'}, lesson | {'location': 'СДО'},
                  lesson | {'location': 'А 101', 'subject': 'Другой предмет'},
                  lesson | {'location': 'А 101', 'end': first.replace(hour=11)}):
        pairs.append({'a': {**lesson, 'start': lesson['start'].isoformat(), 'end': lesson['end'].isoformat()},
                      'b': {**other, 'start': other['start'].isoformat(), 'end': other['end'].isoformat()},
                      'expected': schedule.same_lesson(lesson, other)})
    destination = ROOT / 'internal/schedule/testdata/python_calendar.json'
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps({'cases': cases, 'pairs': pairs}, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'Captured {len(cases)} recurrence calendars and {len(pairs)} lesson-matching pairs.')


if __name__ == '__main__':
    main()
