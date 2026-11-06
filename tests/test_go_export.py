import hashlib
import importlib.util
import json
from pathlib import Path
import sqlite3

import pytest

spec = importlib.util.spec_from_file_location('go_export', Path(__file__).parents[1] / 'scripts/export_sqlite_for_go.py')
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)


def source_databases(tmp_path):
    main = tmp_path / 'sessions.sqlite3'
    aaid = tmp_path / 'aaid.sqlite3'
    db = sqlite3.connect(main)
    db.execute('PRAGMA journal_mode=WAL')
    db.executescript('CREATE TABLE config(key TEXT PRIMARY KEY,value TEXT); CREATE TABLE accounts(id TEXT PRIMARY KEY,secret TEXT); CREATE TABLE events(id INTEGER PRIMARY KEY);')
    db.execute('INSERT INTO accounts VALUES(?,?)', ('synthetic', 'encrypted-synthetic-session'))
    db.commit()
    with sqlite3.connect(aaid) as identity:
        identity.executescript('CREATE TABLE users(id TEXT); CREATE TABLE mirea_links(user_id TEXT); CREATE TABLE sessions(id TEXT); CREATE TABLE refresh_tokens(hash TEXT);')
    return main, aaid, db


def test_export_includes_wal_and_has_verifiable_counts_without_changing_source(tmp_path):
    main, aaid, db = source_databases(tmp_path)
    try:
        output = tmp_path / 'test.aa-snapshot.jsonl'
        counts = exporter.export(main, aaid, output)
        lines = output.read_bytes().splitlines(keepends=True)
        records = [json.loads(line) for line in lines]
        summary = records[-1]
        assert summary['sha256'] == hashlib.sha256(b''.join(lines[:-1])).hexdigest()
        assert summary['counts'] == counts and counts['aa.accounts'] == 1
        assert any(record.get('values') == ['synthetic', 'encrypted-synthetic-session'] for record in records)
        assert db.execute('SELECT * FROM accounts').fetchall() == [('synthetic', 'encrypted-synthetic-session')]
        with pytest.raises(FileExistsError):
            exporter.export(main, aaid, output)
    finally:
        db.close()


def test_export_refuses_unknown_tables_instead_of_silently_dropping_data(tmp_path):
    main, aaid, db = source_databases(tmp_path)
    try:
        db.execute('CREATE TABLE future_data(id INTEGER)')
        db.commit()
        output = tmp_path / 'failed.aa-snapshot.jsonl'
        with pytest.raises(ValueError, match='unsupported tables'):
            exporter.export(main, aaid, output)
        assert all(json.loads(line)['type'] != 'summary' for line in output.read_bytes().splitlines())
    finally:
        db.close()
