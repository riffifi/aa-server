"""Group lessons from the public MIREA schedule (schedule-of.mirea.ru)."""
import asyncio
import re
import time
from datetime import datetime, timedelta, timezone
from dateutil.rrule import rrulestr

HOST = 'https://schedule-of.mirea.ru'
MOSCOW = timezone(timedelta(hours=3))
# Online lessons share no room, so they are matched by subject and teacher instead.
DISTANT = re.compile(r'дистанц|онлайн|сдо|online|remote', re.I)

def ical_url(group_id):
    return f'{HOST}/schedule/api/ical/Group/{int(group_id)}?includeMeta=true'

def page_url(group_id):
    return f'{HOST}/?s=1_{int(group_id)}'

def moscow_now():
    return datetime.now(MOSCOW).replace(tzinfo=None)

def epoch(moment):
    """Unix time of a Moscow civil datetime."""
    return moment.replace(tzinfo=MOSCOW).timestamp()

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
                   'type': field('X-META-LESSON_TYPE') or '', 'location': field('LOCATION') or '',
                   'teacher': ', '.join(event.get('X-META-TEACHER', []))}

def current_and_next(lessons, now):
    current = next((lesson for lesson in lessons if lesson['start'] <= now < lesson['end']), None)
    upcoming = next((lesson for lesson in lessons if lesson['start'] > now), None)
    return current, upcoming

def _place(lesson):
    return re.sub(r'\s+', ' ', lesson['location']).strip().casefold()

def same_lesson(a, b):
    """Whether two groups' lessons are one class: same time and room, or for two
    online classes (or ones without a room) the same subject and teacher."""
    if a['start'] != b['start'] or a['end'] != b['end']:
        return False
    room = lambda lesson: bool(_place(lesson)) and not DISTANT.search(_place(lesson))
    if room(a) or room(b):
        return room(a) and room(b) and _place(a) == _place(b)
    return a['subject'].casefold() == b['subject'].casefold() and a['teacher'].casefold() == b['teacher'].casefold()

class Calendars:
    """Group calendars, fetched at most every 15 minutes per group. A failed fetch is
    not retried for a minute, and each day's lessons are parsed once per download."""
    def __init__(self, fetch, ttl=900, retry=60):
        self.fetch, self.ttl, self.retry = fetch, ttl, retry
        self.cache, self.locks, self.failed, self.parsed = {}, {}, {}, {}

    def _fresh(self, group_id):
        cached = self.cache.get(group_id)
        if cached is not None and time.time() - cached[0] < self.ttl:
            return cached
        if time.time() - self.failed.get(group_id, 0) < self.retry:
            return cached or (0, None)
        return None

    async def _download(self, group_id):
        fresh = self._fresh(group_id)
        if fresh is not None:
            return fresh
        async with self.locks.setdefault(group_id, asyncio.Lock()):
            fresh = self._fresh(group_id)
            if fresh is not None:
                return fresh
            try:
                text = await self.fetch(ical_url(group_id))
            except Exception:
                text = None
            if text is None or 'BEGIN:VCALENDAR' not in text or len(text) > 2 * 1024 * 1024:
                # Keep serving a stale calendar while MIREA is unavailable.
                self.failed[group_id] = time.time()
                return self.cache.get(group_id) or (0, None)
            self.failed.pop(group_id, None)
            self.cache[group_id] = (time.time(), text)
            return self.cache[group_id]

    async def source(self, group_id):
        return (await self._download(group_id))[1]

    async def lessons(self, group_id, day):
        fetched, text = await self._download(group_id)
        if text is None:
            return None
        parsed = self.parsed.get(group_id)
        if parsed is None or parsed[:2] != (fetched, day):
            parsed = self.parsed[group_id] = (fetched, day, lessons_on(text, day))
        return [dict(lesson) for lesson in parsed[2]]
