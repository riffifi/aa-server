"""Telegram person grants, with separate app credentials and expiring handoffs."""
import secrets
import time

from .vault import digest


class Agreements:
    def __init__(self, vault):
        self.vault = vault
        with vault.transaction() as db:
            columns = {row[1] for row in db.execute('PRAGMA table_info(telegram_links)')}
            if 'verified' not in columns:
                db.execute('ALTER TABLE telegram_links ADD COLUMN verified INTEGER NOT NULL DEFAULT 0')
            db.executescript('''
                CREATE TABLE IF NOT EXISTS pass_clients(key_hash TEXT PRIMARY KEY,
                    telegram_id INTEGER NOT NULL, label TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS pass_requests(id TEXT PRIMARY KEY,
                    client_hash TEXT NOT NULL, account_id TEXT NOT NULL,
                    telegram_id INTEGER, label TEXT, state TEXT NOT NULL,
                    expires REAL NOT NULL, notified INTEGER NOT NULL DEFAULT 0);
                CREATE INDEX IF NOT EXISTS pass_request_client ON pass_requests(client_hash,account_id);
                CREATE TABLE IF NOT EXISTS pass_grants(id TEXT PRIMARY KEY,
                    account_id TEXT NOT NULL, telegram_id INTEGER NOT NULL,
                    label TEXT NOT NULL, allowed INTEGER NOT NULL,
                    UNIQUE(account_id,telegram_id));
            ''')

    def owners(self, account_id):
        with self.vault.transaction() as db:
            return [r[0] for r in db.execute('SELECT chat_id FROM telegram_links WHERE account_id=? AND verified=1', (account_id,))]

    def owns(self, telegram_id, account_id):
        return telegram_id in self.owners(account_id)

    def verify_owner(self, telegram_id, account_id):
        self.vault.link_telegram(telegram_id, account_id)
        with self.vault.transaction() as db:
            db.execute('UPDATE telegram_links SET verified=1 WHERE chat_id=? AND account_id=?', (telegram_id, account_id))
            self._event(db, account_id)
        self.vault.audit('telegram_owner_verified', 'telegram:' + str(telegram_id), account_id)

    def unlink_owner(self, telegram_id, account_id):
        with self.vault.transaction() as db:
            db.execute('DELETE FROM telegram_links WHERE chat_id=? AND account_id=?', (telegram_id, account_id))
            self._event(db, account_id)
        self.vault.audit('telegram_owner_unlinked', 'telegram:' + str(telegram_id), account_id)

    @staticmethod
    def client_hash(key):
        if not isinstance(key, str) or not 32 <= len(key) <= 128 or not key.isascii():
            return None
        return digest(key)

    def status(self, account_id, key):
        account = self.vault.account(account_id)
        if account is None:
            return 'unavailable'
        if not account['pass_allowed']:
            return 'disabled'
        hashed = self.client_hash(key)
        with self.vault.transaction() as db:
            client = db.execute('SELECT telegram_id FROM pass_clients WHERE key_hash=?', (hashed,)).fetchone()
            if client and self.owns(client[0], account_id):
                return 'approved'
            grant = db.execute('SELECT allowed FROM pass_grants WHERE account_id=? AND telegram_id=?', (account_id, client[0])).fetchone() if client else None
            if grant and grant[0] and self.owners(account_id):
                return 'approved'
            pending = db.execute("SELECT 1 FROM pass_requests WHERE account_id=? AND client_hash=? AND state='pending' AND expires>?", (account_id, hashed, time.time())).fetchone()
            if pending:
                return 'pending'
            if grant and not grant[0]:
                return 'denied'
        return 'not_requested' if self.owners(account_id) else 'owner_unlinked'

    def create(self, account_id, key):
        hashed = self.client_hash(key)
        if hashed is None:
            raise ValueError('Invalid app credential')
        if self.vault.account(account_id) is None:
            raise KeyError(account_id)
        now = time.time()
        with self.vault.transaction() as db:
            db.execute('DELETE FROM pass_requests WHERE expires<?', (now,))
            existing = db.execute("SELECT id FROM pass_requests WHERE client_hash=? AND account_id=? AND state='link' AND expires>?", (hashed, account_id, now)).fetchone()
            if existing:
                return existing[0]
            if db.execute('SELECT COUNT(*) FROM pass_requests').fetchone()[0] >= 10000:
                raise ValueError('Too many requests; retry later')
            identifier = secrets.token_urlsafe(24)
            db.execute("INSERT INTO pass_requests(id,client_hash,account_id,state,expires) VALUES(?,?,?,'link',?)", (identifier, hashed, account_id, now + 600))
            return identifier

    def preview(self, identifier):
        with self.vault.transaction() as db:
            row = db.execute("SELECT * FROM pass_requests WHERE id=? AND state='link' AND expires>?", (identifier, time.time())).fetchone()
            return dict(row) if row else None

    def claim(self, identifier, telegram_id, label):
        now = time.time()
        with self.vault.transaction() as db:
            row = db.execute("SELECT * FROM pass_requests WHERE id=? AND state='link' AND expires>?", (identifier, now)).fetchone()
            if not row or self.vault.account(row['account_id']) is None:
                return None
            client = db.execute('SELECT telegram_id FROM pass_clients WHERE key_hash=?', (row['client_hash'],)).fetchone()
            if client and client[0] != telegram_id:
                return None
            owners = self.owners(row['account_id'])
            grant = db.execute('SELECT allowed FROM pass_grants WHERE account_id=? AND telegram_id=?', (row['account_id'], telegram_id)).fetchone()
            state = 'approved' if telegram_id in owners or (owners and grant and grant[0]) else 'pending'
            db.execute('INSERT OR REPLACE INTO pass_clients VALUES(?,?,?)', (row['client_hash'], telegram_id, label[:160]))
            db.execute('UPDATE pass_requests SET telegram_id=?,label=?,state=?,expires=? WHERE id=?', (telegram_id, label[:160], state, now + 7 * 86400, identifier))
            self._event(db, row['account_id'])
        self.vault.audit('pass_requested', 'telegram:' + str(telegram_id), row['account_id'])
        return state

    def notifications(self):
        with self.vault.transaction() as db:
            return [dict(row) for row in db.execute("SELECT * FROM pass_requests WHERE state='pending' AND notified=0 AND expires>? AND EXISTS (SELECT 1 FROM telegram_links WHERE telegram_links.account_id=pass_requests.account_id AND verified=1) LIMIT 20", (time.time(),))]

    def notified(self, identifier):
        with self.vault.transaction() as db:
            db.execute('UPDATE pass_requests SET notified=1 WHERE id=?', (identifier,))

    def _event(self, db, account_id):
        db.execute('UPDATE accounts SET version=version+1,updated_at=? WHERE id=?', (time.time(), account_id))
        row = db.execute('SELECT version FROM accounts WHERE id=?', (account_id,)).fetchone()
        if row:
            self.vault._event(db, account_id, row[0], 'pass_agreement')

    def decide(self, identifier, owner, allowed):
        with self.vault.transaction() as db:
            request = db.execute("SELECT * FROM pass_requests WHERE id=? AND state='pending' AND expires>?", (identifier, time.time())).fetchone()
            if not request or not self.owns(owner, request['account_id']):
                return False
            existing = db.execute('SELECT id FROM pass_grants WHERE account_id=? AND telegram_id=?', (request['account_id'], request['telegram_id'])).fetchone()
            grant_id = existing[0] if existing else secrets.token_urlsafe(16)
            db.execute('INSERT OR REPLACE INTO pass_grants VALUES(?,?,?,?,?)', (grant_id, request['account_id'], request['telegram_id'], request['label'], int(allowed)))
            db.execute('UPDATE pass_requests SET state=? WHERE account_id=? AND telegram_id=?', ('approved' if allowed else 'denied', request['account_id'], request['telegram_id']))
            self._event(db, request['account_id'])
        self.vault.audit('pass_approved' if allowed else 'pass_denied', 'telegram:' + str(owner), request['account_id'])
        return True

    def grants(self, owner, account_id):
        if not self.owns(owner, account_id):
            return []
        with self.vault.transaction() as db:
            return [dict(row) for row in db.execute('SELECT * FROM pass_grants WHERE account_id=? AND allowed=1', (account_id,))]

    def revoke(self, identifier, owner):
        with self.vault.transaction() as db:
            grant = db.execute('SELECT * FROM pass_grants WHERE id=? AND allowed=1', (identifier,)).fetchone()
            if not grant or not self.owns(owner, grant['account_id']):
                return False
            db.execute('UPDATE pass_grants SET allowed=0 WHERE id=?', (identifier,))
            db.execute("UPDATE pass_requests SET state='denied' WHERE account_id=? AND telegram_id=?", (grant['account_id'], grant['telegram_id']))
            self._event(db, grant['account_id'])
        self.vault.audit('pass_revoked', 'telegram:' + str(owner), grant['account_id'])
        return True

    def granted(self, account_id):
        """Telegram people the owner allowed, for the owner's own settings page."""
        with self.vault.transaction() as db:
            return [dict(row) for row in db.execute('SELECT * FROM pass_grants WHERE account_id=? AND allowed=1', (account_id,))]

    def revoke_by_owner_account(self, identifier, account_id, actor):
        # The owner is signed in with AA ID on the site, which proves ownership directly.
        with self.vault.transaction() as db:
            grant = db.execute('SELECT * FROM pass_grants WHERE id=? AND account_id=? AND allowed=1', (identifier, account_id)).fetchone()
            if not grant:
                return False
            db.execute('UPDATE pass_grants SET allowed=0 WHERE id=?', (identifier,))
            db.execute("UPDATE pass_requests SET state='denied' WHERE account_id=? AND telegram_id=?", (account_id, grant['telegram_id']))
            self._event(db, account_id)
        self.vault.audit('pass_revoked', actor, account_id)
        return True
