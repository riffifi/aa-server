import hashlib
import secrets
import sqlite3
import threading
import time
import uuid
from pathlib import Path

def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()

class Blocked(Exception):
    pass

class Store:
    """AA ID accounts. One person = one MIREA (Pulse) identity = one AA ID.

    Uniqueness is enforced by the schema: `mirea_links.pulse_uid` is UNIQUE (a
    MIREA account can back only one AA ID) and `mirea_links.user_id` is the
    primary key (an AA ID is bound to exactly one MIREA account)."""

    def __init__(self, directory):
        directory = Path(directory)
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.lock = threading.RLock()
        self.db = sqlite3.connect(directory / 'aaid.sqlite3', check_same_thread=False, isolation_level=None)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA foreign_keys=ON')
        self.db.executescript('''
          CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'active',
            created REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS mirea_links(user_id TEXT PRIMARY KEY REFERENCES users(id),
            pulse_uid TEXT UNIQUE NOT NULL, group_id INTEGER, group_name TEXT,
            linked_at REAL NOT NULL, verified_at REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
            device TEXT NOT NULL, created REAL NOT NULL, last_used REAL NOT NULL, expires REAL NOT NULL,
            revoked_at REAL, revoke_reason TEXT);
          CREATE TABLE IF NOT EXISTS refresh_tokens(hash TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id),
            created REAL NOT NULL, used_at REAL);
          CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY AUTOINCREMENT, user_id TEXT, action TEXT NOT NULL,
            session_id TEXT, created REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS web_tokens(hash TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id),
            created REAL NOT NULL);
          CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
          CREATE INDEX IF NOT EXISTS links_group ON mirea_links(group_id);
        ''')
        columns = {row[1] for row in self.db.execute('PRAGMA table_info(sessions)')}
        if 'kind' not in columns:
            # 'app' sessions hold tokens for AntiAttendance servers; 'web' sessions only
            # open the AA ID site and are limited separately.
            self.db.execute("ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'app'")

    def audit(self, action, user_id=None, session_id=None):
        self.db.execute('INSERT INTO audit(user_id, action, session_id, created) VALUES (?, ?, ?, ?)',
                        (user_id, action, session_id, time.time()))

    def user(self, user_id):
        return self.db.execute('''SELECT users.id, users.status, users.created, mirea_links.pulse_uid,
            mirea_links.group_id, mirea_links.group_name, mirea_links.verified_at
            FROM users JOIN mirea_links ON mirea_links.user_id = users.id WHERE users.id = ?''', (user_id,)).fetchone()

    def bind_mirea(self, pulse_uid, group_id, group_name):
        """Find the AA ID owned by this MIREA person, creating it on first sign-in."""
        now = time.time()
        with self.lock:
            self.db.execute('BEGIN IMMEDIATE')
            try:
                row = self.db.execute('SELECT user_id FROM mirea_links WHERE pulse_uid = ?', (pulse_uid,)).fetchone()
                if row is None:
                    user_id = str(uuid.uuid4())
                    self.db.execute('INSERT INTO users(id, created) VALUES (?, ?)', (user_id, now))
                    self.db.execute('INSERT INTO mirea_links VALUES (?, ?, ?, ?, ?, ?)',
                                    (user_id, pulse_uid, group_id, group_name, now, now))
                    self.audit('created', user_id)
                else:
                    user_id = row['user_id']
                    # Keep the last known group unless Pulse did not return one this time.
                    self.db.execute('''UPDATE mirea_links SET verified_at = ?, group_id = COALESCE(?, group_id),
                        group_name = COALESCE(?, group_name) WHERE user_id = ?''', (now, group_id, group_name if group_id else None, user_id))
                self.db.execute('COMMIT')
            except BaseException:
                self.db.execute('ROLLBACK')
                raise
        user = self.user(user_id)
        if user['status'] != 'active':
            raise Blocked(user_id)
        return user

    def create_session(self, user_id, device, ttl, max_sessions, kind='app'):
        """Open a device session. Beyond `max_sessions` active sessions of the same
        kind the least recently used ones are signed out, so one AA ID cannot be
        spread across many phones. Returns (session, token): a refresh token for
        an app session, a cookie token for a web session."""
        now = time.time()
        session_id, token = str(uuid.uuid4()), secrets.token_urlsafe(32)
        with self.lock:
            self.db.execute('BEGIN IMMEDIATE')
            try:
                self.db.execute('INSERT INTO sessions(id, user_id, device, created, last_used, expires, kind) VALUES (?, ?, ?, ?, ?, ?, ?)',
                                (session_id, user_id, device, now, now, now + ttl, kind))
                table = 'web_tokens' if kind == 'web' else 'refresh_tokens'
                self.db.execute(f'INSERT INTO {table}(hash, session_id, created) VALUES (?, ?, ?)', (digest(token), session_id, now))
                active = self.db.execute('''SELECT id FROM sessions WHERE user_id = ? AND kind = ? AND revoked_at IS NULL AND expires > ?
                    ORDER BY last_used DESC, created DESC''', (user_id, kind, now)).fetchall()
                for row in active[max_sessions:]:
                    self._revoke(row['id'], 'evicted', now)
                self.audit('signed_in', user_id, session_id)
                self.db.execute('COMMIT')
            except BaseException:
                self.db.execute('ROLLBACK')
                raise
        return self.session(session_id), token

    def web_session(self, token):
        """The active web session behind a site cookie, or None."""
        if not token:
            return None
        row = self.db.execute('''SELECT sessions.* FROM web_tokens JOIN sessions ON sessions.id = web_tokens.session_id
            WHERE web_tokens.hash = ?''', (digest(token),)).fetchone()
        if row is None or row['revoked_at'] is not None or row['expires'] <= time.time():
            return None
        return row

    def _revoke(self, session_id, reason, now=None):
        self.db.execute('UPDATE sessions SET revoked_at = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL',
                        (now or time.time(), reason, session_id))

    def session(self, session_id):
        return self.db.execute('SELECT * FROM sessions WHERE id = ?', (session_id,)).fetchone()

    def active_session(self, session_id):
        row = self.session(session_id)
        if row is None or row['revoked_at'] is not None or row['expires'] <= time.time():
            return None
        return row

    def rotate(self, refresh, ttl):
        """Exchange a refresh token for a new one. A token that was already used is
        evidence that it was copied, so the whole device session is revoked."""
        now = time.time()
        with self.lock:
            self.db.execute('BEGIN IMMEDIATE')
            try:
                token = self.db.execute('SELECT * FROM refresh_tokens WHERE hash = ?', (digest(refresh),)).fetchone()
                if token is None:
                    self.db.execute('COMMIT')
                    return None, None
                session = self.db.execute('SELECT * FROM sessions WHERE id = ?', (token['session_id'],)).fetchone()
                if token['used_at'] is not None:
                    self._revoke(session['id'], 'refresh_reuse', now)
                    self.audit('refresh_reuse', session['user_id'], session['id'])
                    self.db.execute('COMMIT')
                    return None, None
                if session['revoked_at'] is not None or session['expires'] <= now:
                    self.db.execute('COMMIT')
                    return None, None
                fresh = secrets.token_urlsafe(32)
                self.db.execute('UPDATE refresh_tokens SET used_at = ? WHERE hash = ?', (now, token['hash']))
                self.db.execute('INSERT INTO refresh_tokens(hash, session_id, created) VALUES (?, ?, ?)', (digest(fresh), session['id'], now))
                self.db.execute('UPDATE sessions SET last_used = ?, expires = ? WHERE id = ?', (now, now + ttl, session['id']))
                self.db.execute('COMMIT')
            except BaseException:
                self.db.execute('ROLLBACK')
                raise
        return self.session(session['id']), fresh

    def sessions(self, user_id):
        return self.db.execute('''SELECT id, device, kind, created, last_used FROM sessions
            WHERE user_id = ? AND revoked_at IS NULL AND expires > ? ORDER BY last_used DESC''', (user_id, time.time())).fetchall()

    def revoke(self, user_id, session_id, reason):
        with self.lock:
            changed = self.db.execute('UPDATE sessions SET revoked_at = ?, revoke_reason = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL',
                                      (time.time(), reason, session_id, user_id)).rowcount
            if changed:
                self.audit('revoked:' + reason, user_id, session_id)
            return bool(changed)

    def set_status(self, user_id, status):
        with self.lock:
            changed = self.db.execute('UPDATE users SET status = ? WHERE id = ?', (status, user_id)).rowcount
            if changed and status != 'active':
                for row in self.db.execute('SELECT id FROM sessions WHERE user_id = ? AND revoked_at IS NULL', (user_id,)).fetchall():
                    self._revoke(row['id'], 'blocked')
            if changed:
                self.audit('status:' + status, user_id)
            return bool(changed)

    def close(self):
        self.db.close()
