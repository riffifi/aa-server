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
        self.lock = threading.RLock()
        self.db = sqlite3.connect(directory / 'sessions.sqlite3', check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.executescript('''
          CREATE TABLE IF NOT EXISTS config(key TEXT PRIMARY KEY, value TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY, pulse_id TEXT UNIQUE NOT NULL,
            label TEXT NOT NULL, group_id INTEGER, group_name TEXT, secret TEXT NOT NULL,
            state TEXT NOT NULL, expires_at REAL, checked_at REAL NOT NULL, version INTEGER NOT NULL,
            pass_version INTEGER NOT NULL DEFAULT 0, updated_at REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS panel_sessions(key_hash TEXT PRIMARY KEY, csrf TEXT NOT NULL, expires REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL,
            version INTEGER NOT NULL, kind TEXT NOT NULL, created REAL NOT NULL);
        ''')
        columns = {row[1] for row in self.db.execute('PRAGMA table_info(accounts)')}
        if 'session_version' not in columns:
            self.db.execute('ALTER TABLE accounts ADD COLUMN session_version INTEGER NOT NULL DEFAULT 1')
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

    def accounts(self):
        with self.transaction() as db:
            return [dict(row) for row in db.execute('SELECT * FROM accounts ORDER BY label')]

    def account(self, account_id):
        with self.transaction() as db:
            row = db.execute('SELECT * FROM accounts WHERE id=?', (account_id,)).fetchone()
            return dict(row) if row else None

    def public(self, row):
        return {key: row[key] for key in ('id', 'label', 'group_id', 'group_name', 'state', 'expires_at', 'checked_at', 'version', 'session_version', 'pass_version', 'updated_at')}

    def _event(self, db, account_id, version, kind):
        db.execute('INSERT INTO events(account_id,version,kind,created) VALUES(?,?,?,?)', (account_id, version, kind, time.time()))
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
                db.execute('UPDATE accounts SET label=?,group_id=COALESCE(?,group_id),group_name=COALESCE(?,group_name),secret=?,state=?,expires_at=?,checked_at=?,version=?,session_version=session_version+1,updated_at=? WHERE id=?',
                    (label or row['label'], group_id, group_name, self.seal(secret), 'valid', credentials.get('expires_at'), time.time(), version, time.time(), account_id))
            else:
                if expected not in (None, 0) or session_expected not in (None, 0):
                    raise Conflict('Account does not exist')
                account_id, version = str(uuid.uuid4()), 1
                db.execute('INSERT INTO accounts(id,pulse_id,label,group_id,group_name,secret,state,expires_at,checked_at,version,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)',
                    (account_id, pulse_id, label or 'Account', group_id, group_name, self.seal(credentials), 'valid', credentials.get('expires_at'), time.time(), version, time.time()))
            self._event(db, account_id, version, 'session')
        return self.account(account_id)

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
            db.execute('DELETE FROM accounts WHERE id=?', (account_id,))
            self._event(db, account_id, 0, 'deleted')

    def events(self, after):
        with self.transaction() as db:
            rows = db.execute('SELECT * FROM events WHERE id>? ORDER BY id LIMIT 1000', (after,)).fetchall()
            cursor = rows[-1]['id'] if rows else after
            values = [dict(row) for row in rows]
            return values, cursor

    def cursor(self):
        with self.transaction() as db:
            return db.execute('SELECT COALESCE(MAX(id),0) FROM events').fetchone()[0]

    def close(self):
        self.db.close()
