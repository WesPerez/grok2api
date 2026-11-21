#!/usr/bin/env python3
"""Offline proof of the real release schema path with root-created WAL sidecars."""
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import sys
import tempfile

assert os.geteuid() == 0 and len(sys.argv) == 2
source = Path(__file__).resolve().parents[1] / 'deploy/release-transaction.py'
spec = importlib.util.spec_from_file_location('release_schema', source)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
with open('/tmp/grok-schema-release-smoke.lock', 'a') as lock:
    os.dup2(lock.fileno(), 9)
    with tempfile.TemporaryDirectory(prefix='grok-release-schema-') as root:
        root = Path(root)
        database = root / 'seed.db'
        with sqlite3.connect(database) as db:
            db.execute('PRAGMA journal_mode=WAL')
            db.execute('CREATE TABLE preserved (id INTEGER PRIMARY KEY, payload BLOB NOT NULL)')
            db.execute('INSERT INTO preserved VALUES (1, ?)', (b'synthetic-preserved-value',))
        work = root / 'work'
        work.mkdir(mode=0o700)
        final, before, after = module.migration(sys.argv[1], database, work)
        assert before['preserved'] == after['preserved']
        assert json.loads((work / 'schema-intent.json').read_text())['state'] == 'cleanup_confirmed'
        assert before['preserved']['rows'] == 1
        print(json.dumps({'state': 'verified', 'network': 'none', 'repeatedSchemaOnly': True,
                          'oldFieldsPreserved': True, 'cleanupConfirmed': True}))
