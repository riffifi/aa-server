"""The group's lessons from the public MIREA schedule (schedule-of.mirea.ru)."""
import re
from datetime import datetime, timedelta
from dateutil.rrule import rrulestr

HOST = 'https://schedule-of.mirea.ru'

def ical_url(group_id):
    return f'{HOST}/schedule/api/ical/Group/{int(group_id)}?includeMeta=true'

def page_url(group_id):
    return f'{HOST}/?s=1_{int(group_id)}'

def _date(text):
    match = re.fullmatch(r'(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})?Z?', text or '')
    if not match:
        return None
    year, month, day, hour, minute, second = match.groups()
    return datetime(int(year), int(month), int(day), int(hour), int(minute), int(second or 0))

def _unescape(text):
    for escaped, plain in ((r'\n', '\n'), (r'\N', '\n'), (r'\,', ','), (r'\;', ';'), ('\\\\', '\\')):
        text = text.replace(escaped, plain)
    return text

def lessons_on(source, day):
    """Lessons on `day` (a date), in Moscow civil time, sorted by start.
    Mirrors the app's parser: DTSTART/DTEND plus RRULE, RDATE and EXDATE."""
    start_of_day = datetime(day.year, day.month, day.day)
    end_of_day = start_of_day + timedelta(days=1)
    result = {}
    event = None
    for line in re.sub(r'\r?\n[ \t]', '', source).splitlines():
        if line == 'BEGIN:VEVENT':
            event = {}
            continue
        if line == 'END:VEVENT':
            if event is not None:
                for lesson in _expand(event, start_of_day, end_of_day):
                    result[(lesson['start'], lesson['subject'])] = lesson
            event = None
            continue
        if event is None or ':' not in line:
            continue
        key, value = line.split(':', 1)
        event.setdefault(key.split(';')[0].upper(), []).append(_unescape(value))
    return sorted(result.values(), key=lambda lesson: lesson['start'])

def _expand(event, start_of_day, end_of_day):
    field = lambda key: (event.get(key) or [None])[0]
    if field('TRANSP') == 'TRANSPARENT':
        return
    base_start, base_end = _date(field('DTSTART')), _date(field('DTEND'))
    if base_start is None or base_end is None:
        return
    duration = base_end - base_start
    if not timedelta(0) < duration <= timedelta(days=1):
        return
    subject = field('X-META-DISCIPLINE') or field('SUMMARY') or ''
    if not subject:
        return
    starts = set()
    rule = field('RRULE')
    if rule:
        try:
            starts.update(rrulestr(re.sub(r'UNTIL=(\d{8}T\d{6})Z', r'UNTIL=\1', rule), dtstart=base_start)
                          .between(start_of_day, end_of_day, inc=True))
        except (ValueError, TypeError):
            return
    else:
        starts.add(base_start)
    for raw in event.get('RDATE', []):
        starts.update(filter(None, map(_date, raw.split(','))))
    excluded = {value for raw in event.get('EXDATE', []) for value in map(_date, raw.split(',')) if value}
    for start in starts:
        if start_of_day <= start < end_of_day and start not in excluded:
            yield {'start': start, 'end': start + duration, 'subject': subject,
                   'type': field('X-META-LESSON_TYPE') or '', 'location': field('LOCATION') or ''}

def current_and_next(lessons, now):
    current = next((lesson for lesson in lessons if lesson['start'] <= now < lesson['end']), None)
    upcoming = next((lesson for lesson in lessons if lesson['start'] > now), None)
    return current, upcoming
