"""What the admin panel shows for debugging: recent server logs, recent requests,
read-only database tables with secrets hidden, and the live state of the service."""
import logging
import os
import platform
import re
import sys
import time
import traceback
from collections import deque

LOG_LIMIT = 3000
REQUEST_LIMIT = 1000
# Columns whose values never leave the server, whatever the table.
HIDDEN = re.compile(r'(^|_)(secret|key_hash|csrf|password|token|cookie|capability_hash|client_hash)($|_)')
LEVELS = {'debug': 10, 'info': 20, 'warning': 30, 'error': 40}

class LogBuffer(logging.Handler):
    """The last LOG_LIMIT records of the server's own loggers, kept in memory."""
    def __init__(self):
        super().__init__(logging.INFO)
        self.records, self.next_id = deque(maxlen=LOG_LIMIT), 1

    def emit(self, record):
        try:
            message = record.getMessage()
        except Exception:
            message = str(record.msg)
        detail = ''.join(traceback.format_exception(*record.exc_info)) if record.exc_info else ''
        self.records.append({'id': self.next_id, 'time': record.created, 'level': record.levelname.lower(),
                             'logger': record.name, 'message': message[:4000], 'detail': detail[-8000:]})
        self.next_id += 1

    def query(self, level='', q='', after=0, limit=500):
        floor = LEVELS.get(level, 0)
        q = q.casefold()
        rows = [r for r in self.records if r['id'] > after and LEVELS.get(r['level'], 50) >= floor
                and (not q or q in (r['logger'] + ' ' + r['message'] + ' ' + r['detail']).casefold())]
        return rows[-limit:][::-1]

BUFFER = LogBuffer()

class RequestLog:
    """Recent requests and per-route totals: count, errors, time."""
    def __init__(self):
        self.recent, self.routes, self.started = deque(maxlen=REQUEST_LIMIT), {}, time.time()

    def add(self, method, route, path, status, duration):
        self.recent.append({'time': time.time(), 'method': method, 'route': route, 'path': path[:300],
                            'status': status, 'ms': round(duration * 1000, 1)})
        key = method + ' ' + route
        stats = self.routes.setdefault(key, {'route': key, 'count': 0, 'errors': 0, 'total_ms': 0.0, 'max_ms': 0.0, 'last': 0})
        stats['count'] += 1
        stats['errors'] += status >= 500
        stats['total_ms'] += duration * 1000
        stats['max_ms'] = max(stats['max_ms'], duration * 1000)
        stats['last'] = time.time()

    def summary(self, q='', errors=False, slow=0, limit=300):
        q = q.casefold()
        recent = [r for r in self.recent if (not q or q in r['path'].casefold())
                  and (not errors or r['status'] >= 400) and r['ms'] >= slow]
        routes = sorted(({**s, 'avg_ms': round(s['total_ms'] / s['count'], 1), 'max_ms': round(s['max_ms'], 1),
                          'total_ms': round(s['total_ms'])} for s in self.routes.values()),
                        key=lambda s: -s['total_ms'])
        return {'since': self.started, 'recent': recent[-limit:][::-1], 'routes': routes}

def install_logging(buffer):
    # The app's own loggers report INFO and up into the panel; uvicorn's errors too.
    for name in ('app', 'aa', 'aaid', 'uvicorn.error'):
        logger = logging.getLogger(name)
        if buffer not in logger.handlers:
            logger.addHandler(buffer)
        if name in ('app', 'aa') and (logger.level == logging.NOTSET or logger.level > logging.INFO):
            logger.setLevel(logging.INFO)

def _hidden(table, column, value):
    if value is None:
        return None
    if HIDDEN.search(column.lower()) or (table == 'config' and column == 'value'):
        return '•••'
    if isinstance(value, bytes):
        return f'<{len(value)} байт>'
    return value

def tables(db):
    names = [row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
    result = []
    for name in names:
        columns = [row[1] for row in db.execute(f'PRAGMA table_info("{name}")')]
        count = db.execute(f'SELECT COUNT(*) FROM "{name}"').fetchone()[0]
        result.append({'name': name, 'columns': columns, 'rows': count})
    return result

def table(db, name, offset=0, limit=100, q=''):
    known = {t['name']: t['columns'] for t in tables(db)}
    if name not in known:
        return None
    columns = known[name]
    searchable = [c for c in columns if not HIDDEN.search(c.lower()) and not (name == 'config' and c == 'value')]
    where, args = '', []
    if q and searchable:
        where = ' WHERE ' + ' OR '.join(f'CAST("{c}" AS TEXT) LIKE ?' for c in searchable)
        args = ['%' + q + '%'] * len(searchable)
    total = db.execute(f'SELECT COUNT(*) FROM "{name}"{where}', args).fetchone()[0]
    rows = db.execute(f'SELECT * FROM "{name}"{where} ORDER BY rowid DESC LIMIT ? OFFSET ?', [*args, limit, offset]).fetchall()
    return {'name': name, 'columns': columns, 'total': total, 'offset': offset,
            'rows': [[_hidden(name, c, row[c]) for c in columns] for row in rows]}

def files(directory):
    result = {}
    for name in ('sessions.sqlite3', 'sessions.sqlite3-wal'):
        try:
            result[name] = os.path.getsize(os.path.join(directory, name))
        except OSError:
            pass
    return result

def process():
    info = {'python': sys.version.split()[0], 'platform': platform.platform(), 'pid': os.getpid()}
    try:
        with open('/proc/self/status') as stream:
            for line in stream:
                if line.startswith('VmRSS:'):
                    info['memory_kb'] = int(line.split()[1])
        info['load'] = os.getloadavg()
    except (OSError, ValueError):
        pass
    return info
