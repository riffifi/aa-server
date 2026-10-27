import base64
import contextlib
import hashlib
import hmac
import json
import os
import re
import secrets
import sqlite3
import threading
import time
import uuid
from pathlib import Path
from cryptography.fernet import Fernet

class Conflict(Exception):
    pass

def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()

def password_hash(password, salt=None):
    salt = salt or secrets.token_hex(16)
    key = hashlib.scrypt(password.encode(), salt=bytes.fromhex(salt), n=16384, r=8, p=1).hex()
    return f'{salt}:{key}'

def password_matches(password, record):
    try:
        return hmac.compare_digest(password_hash(password, record.split(':')[0]), record)
    except (ValueError, TypeError):
        return False

class Vault:
    def __init__(self, directory, password):
        directory = Path(directory)
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        key_file = directory / 'master.key'
        try:
            descriptor = os.open(key_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, 'wb') as stream:
                stream.write(Fernet.generate_key())
        except FileExistsError:
            pass
        self.cipher = Fernet(key_file.read_bytes().strip())
        self.pass_testing_key = hashlib.sha256(b'pass-testing-key\0' + key_file.read_bytes().strip()).digest()
        self.lock = threading.RLock()
        self.db = sqlite3.connect(directory / 'sessions.sqlite3', check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.executescript('''
          CREATE TABLE IF NOT EXISTS config(key TEXT PRIMARY KEY, value TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS audit_events(id INTEGER PRIMARY KEY AUTOINCREMENT,
            action TEXT NOT NULL, account_id TEXT, actor TEXT NOT NULL, created REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY, pulse_id TEXT UNIQUE NOT NULL,
            label TEXT NOT NULL, group_id INTEGER, group_name TEXT, secret TEXT NOT NULL,
            state TEXT NOT NULL, expires_at REAL, checked_at REAL NOT NULL, version INTEGER NOT NULL,
            pass_version INTEGER NOT NULL DEFAULT 0, updated_at REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS panel_sessions(key_hash TEXT PRIMARY KEY, csrf TEXT NOT NULL, expires REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL,
            version INTEGER NOT NULL, kind TEXT NOT NULL, created REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS telegram_links(chat_id INTEGER NOT NULL, account_id TEXT NOT NULL,
            created REAL NOT NULL, PRIMARY KEY(chat_id, account_id));
          CREATE TABLE IF NOT EXISTS site_sessions(key_hash TEXT PRIMARY KEY, aaid_id TEXT NOT NULL, mirea TEXT NOT NULL,
            group_id INTEGER, group_name TEXT, expires REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS marks(id INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL,
            state TEXT NOT NULL, message TEXT NOT NULL, source TEXT NOT NULL, created REAL NOT NULL);
          CREATE INDEX IF NOT EXISTS marks_account ON marks(account_id, created);
          DROP TABLE IF EXISTS page_sessions;
        ''')
        columns = {row[1] for row in self.db.execute('PRAGMA table_info(accounts)')}
        if 'session_version' not in columns:
            self.db.execute('ALTER TABLE accounts ADD COLUMN session_version INTEGER NOT NULL DEFAULT 1')
        if 'pass_allowed' not in columns:
            # Per-account permission to use the digital pass in the app. Existing
            # accounts keep it on; new accounts are created with it off (see upsert).
            self.db.execute('ALTER TABLE accounts ADD COLUMN pass_allowed INTEGER NOT NULL DEFAULT 1')
        if 'page_seen_at' not in columns:
            # Last time the person signed in to the site (the "has logged in" indicator
            # in the panel). NULL means they never have.
            self.db.execute('ALTER TABLE accounts ADD COLUMN page_seen_at REAL')
        event_columns = {row[1] for row in self.db.execute('PRAGMA table_info(events)')}
        for column in ('group_id', 'previous_group_id'):
            if column not in event_columns:
                self.db.execute(f'ALTER TABLE events ADD COLUMN {column} INTEGER')
        self.db.execute('CREATE INDEX IF NOT EXISTS accounts_group ON accounts(group_id)')
        with self.lock, self.db:
            if self.db.execute("SELECT 1 FROM config WHERE key='admin_password'").fetchone() is None:
                if not password or len(password) < 16:
                    raise ValueError('ADMIN_PASSWORD must contain at least 16 characters for first startup')
                self.db.execute("INSERT INTO config VALUES('admin_password', ?)", (password_hash(password),))
        os.chmod(directory / 'sessions.sqlite3', 0o600)

    @contextlib.contextmanager
    def transaction(self):
        with self.lock, self.db:
            yield self.db

    def seal(self, value):
        return self.cipher.encrypt(json.dumps(value, separators=(',', ':')).encode()).decode()

    def secret(self, account):
        return json.loads(self.cipher.decrypt(account['secret'].encode()))

    def admin_login(self, password):
        with self.transaction() as db:
            record = db.execute("SELECT value FROM config WHERE key='admin_password'").fetchone()[0]
            if not password_matches(password, record):
                return None
            token, csrf = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
            db.execute('DELETE FROM panel_sessions WHERE expires < ?', (time.time(),))
            db.execute('INSERT INTO panel_sessions VALUES(?,?,?)', (digest(token), csrf, time.time() + 43200))
            return token, csrf

    def panel_session(self, token):
        with self.transaction() as db:
            return db.execute('SELECT * FROM panel_sessions WHERE key_hash=? AND expires>?', (digest(token), time.time())).fetchone()

    def logout(self, token):
        with self.transaction() as db:
            db.execute('DELETE FROM panel_sessions WHERE key_hash=?', (digest(token),))

    def accounts(self, group_id=None):
        with self.transaction() as db:
            return [dict(row) for row in db.execute('SELECT * FROM accounts WHERE (? IS NULL OR group_id=?) ORDER BY label', (group_id, group_id))]

    def account(self, account_id):
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            return dict(row) if row else None

    def public(self, row):
        data = {key: row[key] for key in ('id', 'label', 'group_id', 'group_name', 'state', 'expires_at', 'checked_at', 'version', 'session_version', 'pass_version', 'updated_at')}
        data['pass_allowed'] = bool(row['pass_allowed']) if 'pass_allowed' in row.keys() else True
        return data

    def _event(self, db, account_id, version, kind, previous_group_id=None):
        row = db.execute('SELECT group_id FROM accounts WHERE id=?', (account_id,)).fetchone()
        db.execute('INSERT INTO events(account_id,version,kind,created,group_id,previous_group_id) VALUES(?,?,?,?,?,?)', (account_id, version, kind, time.time(), row['group_id'] if row else None, previous_group_id))
        db.execute('DELETE FROM events WHERE created < ?', (time.time() - 7 * 86400,))

    def upsert(self, pulse_id, label, credentials, account_id=None, expected=None, group_id=None, group_name=None, session_expected=None):
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE pulse_id=?', (pulse_id,)).fetchone()
            if account_id and (row is None or row['id'] != account_id):
                raise Conflict('This login belongs to a different Pulse account')
            if row is not None:
                if expected is not None and expected != row['version']:
                    raise Conflict('A newer version is already stored; refresh before publishing')
                if session_expected is not None and session_expected != row['session_version']:
                    raise Conflict('A newer session is already stored')
                secret = self.secret(row)
                # Keep the enrolled identifier across cookie renewal; every
                # activation still obtains a fresh account-bound access token.
                secret.update(credentials)
                account_id = row['id']
                version = row['version'] + 1
                effective_group = group_id if group_id is not None else row['group_id']
                effective_name = group_name if group_name is not None else (row['group_name'] if effective_group == row['group_id'] else None)
                db.execute('UPDATE accounts SET label=?,group_id=?,group_name=?,secret=?,state=?,expires_at=?,checked_at=?,version=?,session_version=session_version+1,updated_at=? WHERE id=?',
                    (label or row['label'], effective_group, effective_name, self.seal(secret), 'valid', credentials.get('expires_at'), time.time(), version, time.time(), account_id))
            else:
                if expected not in (None, 0) or session_expected not in (None, 0):
                    raise Conflict('Account does not exist')
                account_id, version = str(uuid.uuid4()), 1
                # New accounts start without pass permission: the owner turns it on themselves.
                db.execute('INSERT INTO accounts(id,pulse_id,label,group_id,group_name,secret,state,expires_at,checked_at,version,updated_at,pass_allowed) VALUES(?,?,?,?,?,?,?,?,?,?,?,0)',
                    (account_id, pulse_id, label or 'Account', group_id, group_name, self.seal(credentials), 'valid', credentials.get('expires_at'), time.time(), version, time.time()))
            self._event(db, account_id, version, 'session', row['group_id'] if row else None)
        return self.account(account_id)

    def end_session(self, account_id):
        # End the Pulse session without deleting the account: drop the stored cookie
        # and browser state so it can't authenticate (and auto-renewal won't run),
        # mark it expired, and emit a sync event so phones stop using it. The label,
        # group, page password and pass permission are kept.
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            if row is None:
                raise KeyError(account_id)
            secret = self.secret(row)
            for key in ('cookie', 'storage_state', 'expires_at', 'renewed_at'):
                secret.pop(key, None)
            version = row['version'] + 1
            db.execute('UPDATE accounts SET secret=?,state=?,expires_at=NULL,version=?,session_version=session_version+1,updated_at=? WHERE id=?',
                       (self.seal(secret), 'expired', version, time.time(), account_id))
            self._event(db, account_id, version, 'session')
        return self.public(self.account(account_id))

    def rename(self, account_id, label, expected=None):
        label = (label or '').strip()
        if not label or len(label) > 60:
            raise ValueError('Invalid label')
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            if row is None:
                raise KeyError(account_id)
            if expected is not None and expected != row['version']:
                raise Conflict('Account changed; refresh and retry')
            if row['label'] == label:
                return self.public(row)
            version = row['version'] + 1
            db.execute('UPDATE accounts SET label=?,version=?,updated_at=? WHERE id=?', (label, version, time.time(), account_id))
            self._event(db, account_id, version, 'label')
        return self.public(self.account(account_id))

    def update_group(self, account_id, group_id, group_name, expected):
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            if row is None:
                raise KeyError(account_id)
            if row['version'] != expected:
                raise Conflict('Account changed; refresh before assigning a group')
            group_name = group_name.strip() if group_id is not None and group_name else None
            if row['group_id'] == group_id and row['group_name'] == group_name:
                return self.public(row)
            version = row['version'] + 1
            db.execute('UPDATE accounts SET group_id=?,group_name=?,version=?,updated_at=? WHERE id=?',
                       (group_id, group_name, version, time.time(), account_id))
            self._event(db, account_id, version, 'group', row['group_id'])
        return self.public(self.account(account_id))

    def set_pass_allowed(self, account_id, allowed, expected=None):
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            if row is None:
                raise KeyError(account_id)
            if expected is not None and expected != row['version']:
                raise Conflict('Account changed; refresh and retry')
            value = 1 if allowed else 0
            if row['pass_allowed'] == value:
                return self.public(row)
            version = row['version'] + 1
            db.execute('UPDATE accounts SET pass_allowed=?,version=?,updated_at=? WHERE id=?', (value, version, time.time(), account_id))
            self._event(db, account_id, version, 'pass_permission')
        return self.public(self.account(account_id))

    def groups(self):
        with self.transaction() as db:
            return [dict(row) for row in db.execute(
                'SELECT group_id AS id, MAX(group_name) AS name, COUNT(*) AS accounts FROM accounts WHERE group_id IS NOT NULL GROUP BY group_id ORDER BY name')]

    def update_state(self, account_id, state, expected_version):
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            if row is None or row['version'] != expected_version:
                return
            if row['state'] != state:
                version = row['version'] + 1
                db.execute('UPDATE accounts SET state=?,checked_at=?,version=?,updated_at=? WHERE id=?', (state,time.time(),version,time.time(),account_id))
                self._event(db, account_id, version, 'state')
            else:
                db.execute('UPDATE accounts SET checked_at=? WHERE id=?', (time.time(),account_id))

    def save_pass(self, account_id, number, expected=None):
        if not isinstance(number, str) or not re.fullmatch(r'[1-9][0-9]{0,18}', number) or not 0 < int(number) <= 9223372036854775807:
            raise ValueError('Invalid pass identifier')
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            if row is None:
                raise KeyError(account_id)
            if expected is not None and expected != row['pass_version']:
                raise Conflict('A newer pass is already stored')
            secret = self.secret(row)
            if secret.get('pass_number') == number:
                return self.public(row)
            secret['pass_number'] = number
            version, pass_version = row['version'] + 1, row['pass_version'] + 1
            db.execute('UPDATE accounts SET secret=?,version=?,pass_version=?,updated_at=? WHERE id=?', (self.seal(secret),version,pass_version,time.time(),account_id))
            self._event(db, account_id, version, 'pass')
        return self.public(self.account(account_id))

    def delete_account(self, account_id):
        with self.transaction() as db:
            row = db.execute('SELECT group_id FROM accounts WHERE id=?', (account_id,)).fetchone()
            db.execute('DELETE FROM accounts WHERE id=?', (account_id,))
            db.execute('DELETE FROM telegram_links WHERE account_id=?', (account_id,))
            for table in ('pass_requests', 'pass_grants'):
                if db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (table,)).fetchone():
                    db.execute(f'DELETE FROM {table} WHERE account_id=?', (account_id,))
            db.execute('DELETE FROM marks WHERE account_id=?', (account_id,))
            self._event(db, account_id, 0, 'deleted', row['group_id'] if row else None)

    def account_by_pulse(self, pulse_id):
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE pulse_id=?', (pulse_id,)).fetchone()
            return dict(row) if row else None

    def create_site_session(self, aaid_id, mirea, group_id, group_name, ttl=2592000):
        token = secrets.token_urlsafe(32)
        with self.transaction() as db:
            db.execute('DELETE FROM site_sessions WHERE expires < ?', (time.time(),))
            db.execute('INSERT INTO site_sessions VALUES(?,?,?,?,?,?)', (digest(token), aaid_id, mirea, group_id, group_name, time.time() + ttl))
            db.execute('UPDATE accounts SET page_seen_at=? WHERE pulse_id=?', (time.time(), mirea))
        return token

    def site_session(self, token):
        if not token:
            return None
        with self.transaction() as db:
            row = db.execute('SELECT * FROM site_sessions WHERE key_hash=? AND expires>?', (digest(token), time.time())).fetchone()
            return dict(row) if row else None

    def delete_site_session(self, token):
        if token:
            with self.transaction() as db:
                db.execute('DELETE FROM site_sessions WHERE key_hash=?', (digest(token),))

    def record_mark(self, account_id, state, message, source):
        with self.transaction() as db:
            db.execute('INSERT INTO marks(account_id,state,message,source,created) VALUES(?,?,?,?,?)',
                       (account_id, state, message[:200], source, time.time()))
            db.execute('DELETE FROM marks WHERE created < ?', (time.time() - 30 * 86400,))

    def latest_marks(self, account_ids, since):
        """The newest mark of each account since `since`; an approval wins over later failures."""
        if not account_ids:
            return {}
        with self.transaction() as db:
            marks = {}
            for row in db.execute(f'SELECT * FROM marks WHERE created>=? AND account_id IN ({",".join("?" * len(account_ids))}) ORDER BY created',
                                  (since, *account_ids)):
                previous = marks.get(row['account_id'])
                if previous is None or previous['state'] != 'approved':
                    marks[row['account_id']] = dict(row)
            return marks

    def link_telegram(self, chat_id, account_id):
        with self.transaction() as db:
            if db.execute('SELECT 1 FROM accounts WHERE id=?', (account_id,)).fetchone() is None:
                raise KeyError(account_id)
            db.execute('INSERT OR IGNORE INTO telegram_links(chat_id,account_id,created) VALUES(?,?,?)', (chat_id, account_id, time.time()))

    def unlink_telegram(self, chat_id, account_id):
        with self.transaction() as db:
            db.execute('DELETE FROM telegram_links WHERE chat_id=? AND account_id=?', (chat_id, account_id))

    def telegram_accounts(self, chat_id):
        with self.transaction() as db:
            return [row[0] for row in db.execute('SELECT account_id FROM telegram_links WHERE chat_id=?', (chat_id,))]

    def telegram_chats(self, account_id):
        with self.transaction() as db:
            return [row[0] for row in db.execute('SELECT chat_id FROM telegram_links WHERE account_id=?', (account_id,))]

    def events(self, after, group_id=None):
        with self.transaction() as db:
            rows = db.execute('SELECT * FROM events WHERE id>? ORDER BY id LIMIT 1000', (after,)).fetchall()
            cursor = rows[-1]['id'] if rows else after
            values = [dict(row) for row in rows if group_id is None or row['group_id'] == group_id or row['previous_group_id'] == group_id]
            return values, cursor

    def cursor(self):
        with self.transaction() as db:
            return db.execute('SELECT COALESCE(MAX(id),0) FROM events').fetchone()[0]

    def close(self):
        self.db.close()

    def audit(self, action, actor, account_id=None):
        with self.transaction() as db:
            db.execute('INSERT INTO audit_events(action,account_id,actor,created) VALUES(?,?,?,?)',
                       (action, account_id, actor, time.time()))
            db.execute('DELETE FROM audit_events WHERE created<?', (time.time() - 30 * 86400,))
            db.execute('DELETE FROM audit_events WHERE id NOT IN (SELECT id FROM audit_events ORDER BY id DESC LIMIT 10000)')
