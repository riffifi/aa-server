"""Export read-only SQLite snapshots for the Go/PostgreSQL importer.

Output contains encrypted sessions and credential hashes. It is written only to
the explicitly named file, created with mode 0600; existing files are refused.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import sys
import tempfile


TABLES = {
    'aa': ('config', 'accounts', 'audit_events', 'panel_sessions', 'events', 'telegram_links',
           'site_sessions', 'marks', 'pass_shares', 'telegram_users', 'telegram_codes',
           'share_requests', 'pass_clients', 'pass_requests', 'pass_grants', 'devices'),
    'aaid': ('users', 'mirea_links', 'sessions', 'refresh_tokens', 'web_tokens', 'audit'),
}
REQUIRED = {'aa': {'config', 'accounts', 'events'}, 'aaid': {'users', 'mirea_links', 'sessions', 'refresh_tokens'}}


def export(sessions, aaid, output):
    digest = hashlib.sha256()
    counts = {}
    # Reserve the destination before reading source databases. Never overwrite it.
    descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, 'wb') as stream:
            def emit(record, hashed=True):
                line = json.dumps(record, ensure_ascii=False, separators=(',', ':'), allow_nan=False).encode('utf-8') + b'\n'
                stream.write(line)
                if hashed:
                    digest.update(line)
            emit({'type': 'header', 'version': 1})
            for schema, source_path in (('aa', sessions), ('aaid', aaid)):
                with tempfile.TemporaryDirectory(prefix='aa-export-') as directory:
                    # sqlite backup includes committed WAL data in a consistent snapshot.
                    with sqlite3.connect(Path(source_path).resolve().as_uri() + '?mode=ro', uri=True) as source:
                        snapshot = sqlite3.connect(Path(directory) / 'snapshot.sqlite3')
                        try:
                            source.backup(snapshot)
                            existing = {row[0] for row in snapshot.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")}
                            missing = REQUIRED[schema] - existing
                            if missing:
                                raise ValueError(f'{schema}: missing required tables: {sorted(missing)}')
                            unknown = existing - set(TABLES[schema])
                            if unknown:
                                raise ValueError(f'{schema}: unsupported tables: {sorted(unknown)}')
                            for table in TABLES[schema]:
                                if table not in existing:
                                    continue
                                columns = [row[1] for row in snapshot.execute(f'PRAGMA table_info("{table}")')]
                                # Early Python releases left this obsolete authorization
                                # table behind. Preserve it outside both runtime schemas;
                                # importing it must never reactivate legacy device keys.
                                target_schema = 'aa_legacy' if schema == 'aa' and table == 'devices' else schema
                                emit({'type': 'table', 'schema': target_schema, 'name': table, 'columns': columns})
                                count = 0
                                for row in snapshot.execute(f'SELECT * FROM "{table}"'):
                                    emit({'type': 'row', 'values': list(row)})
                                    count += 1
                                counts[target_schema + '.' + table] = count
                        finally:
                            snapshot.close()
            emit({'type': 'summary', 'counts': counts, 'sha256': digest.hexdigest()}, hashed=False)
            stream.flush()
            os.fsync(stream.fileno())
    except BaseException:
        # Keep a failed export distinguishable: it has no valid terminal summary.
        raise
    return counts


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sessions', required=True, type=Path)
    parser.add_argument('--aaid', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    try:
        counts = export(args.sessions, args.aaid, args.output)
    except (ValueError, OSError, sqlite3.Error) as error:
        print(f'Export failed: {error}. Source databases were not changed.', file=sys.stderr)
        raise SystemExit(1) from None
    archived = counts.get('aa_legacy.devices')
    if archived is not None:
        print(f'Legacy devices: {archived} rows archived as aa_legacy.devices; legacy device keys remain disabled.')
    print(json.dumps({'tables': len(counts), 'rows': sum(counts.values())}))


if __name__ == '__main__':
    main()
