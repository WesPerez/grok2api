#!/usr/bin/env python3
"""Exact hosted image, one registered lock, offline schema proof and live acceptance."""
import base64
import hmac
from contextlib import closing
import io
import tarfile
import zipfile
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = Path('/root/grok2api')
LOCK = '/run/lock/grok2api-deploy.lock'
STATE = Path('/var/lib/grok2api-deploy')
DATA = Path('/var/lib/docker/volumes/grok2api_grok2api-data/_data')
QUALITY = Path('/var/lib/docker/volumes/grok2api_quality_guard_state/_data')
COMPOSE = ['docker', '--host', 'unix:///var/run/docker.sock', 'compose', '--project-name', 'grok2api', '--env-file', '.env', '-f', 'docker-compose.yml']


def command(args, **kwargs):
    kwargs.setdefault('timeout', 120)
    kwargs.setdefault('capture_output', True)
    kwargs.setdefault('text', True)
    return subprocess.run(args, check=True, pass_fds=(9,), **kwargs).stdout


def atomic(path, value):
    path = Path(path)
    temporary = path.with_name(path.name + '.next')
    raw = value if isinstance(value, bytes) else (json.dumps(value, indent=2) + '\n').encode()
    with temporary.open('wb') as stream:
        os.chmod(temporary, 0o600)
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def inspect(name):
    return json.loads(command(['docker', 'inspect', name]))[0]


def snapshot(source, destination):
    with closing(sqlite3.connect('file:' + str(source) + '?mode=ro', uri=True)) as old:
        with closing(sqlite3.connect(destination)) as new:
            old.backup(new)
            assert new.execute('PRAGMA integrity_check').fetchone()[0] == 'ok'
            assert not new.execute('PRAGMA foreign_key_check').fetchall()
            new.execute('PRAGMA wal_checkpoint(TRUNCATE)')
    with open(destination, 'rb') as complete:
        os.fsync(complete.fileno())


def fingerprint(path, columns=None):
    """Hash every old column, never serialize plaintext credential material."""
    result = {}
    with closing(sqlite3.connect('file:' + str(path) + '?mode=ro', uri=True)) as db:
        db.execute('BEGIN')
        assert db.execute('PRAGMA integrity_check').fetchone()[0] == 'ok'
        assert not db.execute('PRAGMA foreign_key_check').fetchall()
        tables = sorted(row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'"))
        if columns is not None:
            assert set(columns) <= set(tables), 'Migration removed an old table'
            tables = sorted(columns)
        for table in tables:
            quote = lambda s: '"' + s.replace('"', '""') + '"'
            existing = [x[1] for x in db.execute('PRAGMA table_info(' + quote(table) + ')')]
            fields = columns[table]['columns'] if columns is not None else existing
            assert set(fields) <= set(existing), 'Migration removed an old column'
            rows = []
            for row in db.execute('SELECT ' + ','.join(map(quote, fields)) + ' FROM ' + quote(table)):
                encoded = []
                for value in row:
                    if isinstance(value, bytes): encoded.append(['bytes', value.hex()])
                    else: encoded.append([type(value).__name__, value])
                rows.append(json.dumps(encoded, ensure_ascii=False, separators=(',', ':')).encode())
            digest = hashlib.sha256()
            for row in sorted(rows):
                digest.update(str(len(row)).encode() + b':' + row)
            result[table] = {'columns': fields, 'rows': len(rows), 'sha256': digest.hexdigest()}
    return result


def schema_permissions(directory):
    # Read-only SQLite checks can create WAL/SHM owned by the host inspector.
    # The offline schema worker must own every database file before each start.
    os.chown(directory, 10001, 10001)
    for name in ('backend.db', 'backend.db-wal', 'backend.db-shm'):
        path = directory / name
        if path.exists():
            os.chown(path, 10001, 10001)
            os.chmod(path, 0o600)


def migration(image, source, work):
    directory = work / 'data'
    directory.mkdir(mode=0o700)
    db = directory / 'backend.db'
    snapshot(source, db)
    before = fingerprint(db)
    config = work / 'config.yaml'
    config.write_text('database:\n  driver: sqlite\n  sqlite:\n    path: /app/data/backend.db\nsecrets:\n  jwtSecret: ' + secrets.token_hex(32) + '\n  credentialEncryptionKey: ' + base64.b64encode(secrets.token_bytes(32)).decode() + '\n')
    config.chmod(0o444)
    schema_permissions(directory)
    name = 'grok-schema-' + secrets.token_hex(8)
    args = ['docker', 'run', '--name', name, '--network', 'none', '--pull', 'never', '--user', '10001:10001', '--entrypoint', '/app/grok2api', '--env', 'GROK2API_DATABASE_URL=', '--mount', f'type=bind,src={directory},dst=/app/data', '--mount', f'type=bind,src={config},dst=/run/schema.yaml,readonly', image, '--config', '/run/schema.yaml', '--schema-only']
    atomic(work / 'schema-intent.json', {'container': name, 'image': image, 'state': 'running'})
    try:
        command(args, timeout=120)
        first = fingerprint(db, before)
        assert first == before, 'Schema migration changed existing fields'
        complete = fingerprint(db)
        schema_permissions(directory)
        command(['docker', 'start', '--attach', name], timeout=120)
        assert fingerprint(db) == complete, 'Repeated initialization is not idempotent'
        with closing(sqlite3.connect(db)) as connection:
            info = {row[1]: row for row in connection.execute('PRAGMA table_info(account_credentials)')}
            for field in ('console_api_key_id', 'encrypted_console_workos_session'):
                assert info[field][3] == 1 and info[field][4] in ("''", '""'), 'Invalid cache default'
                assert connection.execute('SELECT count(*) FROM account_credentials WHERE "' + field + '" != \'\' OR "' + field + '" IS NULL').fetchone()[0] == 0
        return db, before, complete
    finally:
        # Only this exact schema-only container is stopped and removed.
        subprocess.run(['docker', 'stop', '--time', '5', name], capture_output=True, timeout=15, pass_fds=(9,))
        command(['docker', 'rm', name], timeout=30)
        state = subprocess.run(['docker', 'inspect', name], capture_output=True, timeout=10, pass_fds=(9,))
        assert state.returncode != 0 and b'no such' in state.stderr.lower(), 'Schema cleanup unconfirmed'
        atomic(work / 'schema-intent.json', {'container': name, 'image': image, 'state': 'cleanup_confirmed'})


def topology():
    current = inspect('grok2api')
    model = json.loads(command(COMPOSE + ['config', '--format', 'json']))
    service = model['services']['grok2api']
    ports = {str(p['target']) + '/tcp': [{'HostIp': p.get('host_ip', '0.0.0.0'), 'HostPort': str(p['published'])}] for p in service['ports']}
    assert ports == current['HostConfig']['PortBindings'], 'Port drift'
    volumes = model.get('volumes', {})
    mounts = sorted((v['type'], v['source'] if v['type'] == 'bind' else volumes[v['source']]['name'], v['target'], not v.get('read_only', False)) for v in service['volumes'])
    actual = sorted((m['Type'], m['Source'] if m['Type'] == 'bind' else m['Name'], m['Destination'], m['RW']) for m in current['Mounts'])
    assert mounts == actual, 'Data mount drift'
    expected = {model['networks'][name]['name']: config.get('ipv4_address') for name, config in service['networks'].items()}
    assert expected == {name: value['IPAddress'] for name, value in current['NetworkSettings']['Networks'].items()}, 'Network drift'
    return current


def health():
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open('http://127.0.0.1:18000/healthz', timeout=5) as response:
        assert response.status == 200 and json.load(response).get('ok') is True


def authenticated_smoke():
    # Use the existing active administrator session; never create or rotate it.
    import yaml
    with closing(sqlite3.connect('file:' + str(DATA / 'backend.db') + '?mode=ro', uri=True)) as db:
        row = db.execute('SELECT id, admin_id, expires_at FROM admin_sessions ORDER BY id DESC LIMIT 1').fetchone()
        assert row, 'Existing administrator session required'
    from datetime import datetime
    expiry = datetime.fromisoformat(row[2].replace('Z', '+00:00'))
    assert expiry.timestamp() > time.time() + 1200, 'Administrator session expires during deployment'
    config = yaml.safe_load((ROOT / 'config.yaml').read_text())
    encode = lambda b: base64.urlsafe_b64encode(b).rstrip(b'=').decode()
    body = {'adminId': row[1], 'sessionId': row[0], 'iss': 'grok2api', 'sub': str(row[1]), 'iat': int(time.time()), 'exp': int(time.time()) + 120}
    unsigned = encode(b'{"alg":"HS256","typ":"JWT"}') + '.' + encode(json.dumps(body).encode())
    token = unsigned + '.' + encode(hmac.new(config['secrets']['jwtSecret'].encode(), unsigned.encode(), hashlib.sha256).digest())
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    request = urllib.request.Request('http://127.0.0.1:18000/api/admin/v1/accounts?page=1&pageSize=1', headers={'Authorization': 'Bearer ' + token})
    with opener.open(request, timeout=10) as response:
        result = json.load(response)
        assert response.status == 200 and isinstance(result['data']['total'], int)
    with opener.open('http://127.0.0.1:18000/', timeout=10) as response:
        html = response.read().decode()
        scripts = re.findall(r'<script[^>]+src="(/grok2api/assets/[^"?]+\.js)"', html)
        assert scripts, 'Frontend entrypoint is missing'
    for script in scripts:
        with opener.open('http://127.0.0.1:18000' + script.removeprefix('/grok2api'), timeout=10) as response:
            assert response.status == 200 and len(response.read()) > 100


def main():
    assert sys.argv[1:] == [str(ROOT), 'docker-compose.yml', 'grok2api', LOCK], 'Fixed transaction contract required'
    assert os.geteuid() == 0 and os.environ.get('GROK_RELEASE_LOCK_FD') == '9'
    assert os.fstat(9).st_ino == os.stat(LOCK).st_ino and os.fstat(9).st_dev == os.stat(LOCK).st_dev
    os.chdir(ROOT)
    for key in ('COMPOSE_FILE', 'COMPOSE_PROJECT_NAME', 'COMPOSE_PROFILES'):
        os.environ.pop(key, None)
    own = Path(__file__).resolve().parent
    identity = json.loads((own / 'identity.json').read_text())
    sha, image = identity['sourceSha'], identity['image']
    assert own == Path('/opt/grok2api/deployment') / sha
    assert re.fullmatch('[a-f0-9]{40}', sha) and re.fullmatch('ghcr.io/wesperez/grok2api@sha256:[a-f0-9]{64}', image)
    assert hashlib.sha256(Path(__file__).read_bytes()).hexdigest() == identity['scriptSha256']
    verified = json.loads(command(['/usr/bin/python3', '-B', '/usr/local/lib/github-ci-fallback/router_client.py', 'verified', '--repo', 'WesPerez/grok2api', '--workflow', '.github/workflows/mine-image.yml', '--ref', 'refs/heads/mine', '--sha', sha, '--request', identity['request'], '--require-job', 'Verify', '--require-job', 'Merge image']))
    assert verified['source']['sha'] == verified['builder']['sha'] == sha
    item = next(x for x in verified['artifacts'] if x['name'] == 'release-tools')
    assert item['digest'] == identity['toolsDigest'], 'Release tool provenance differs'
    raw = subprocess.check_output(['gh', 'api', item['download_path'], '-H', 'Accept: application/octet-stream'], timeout=120, pass_fds=(9,))
    assert len(raw) == item['size'] and 'sha256:' + hashlib.sha256(raw).hexdigest() == item['digest']
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        assert set(archive.namelist()) == {'release-tools.tar.gz'}
        with tarfile.open(fileobj=io.BytesIO(archive.read('release-tools.tar.gz')), mode='r:gz') as tools:
            published = tools.extractfile('release-tools/release-transaction.py').read()
            assert published == Path(__file__).read_bytes(), 'Script is not the verified artifact'
    meta = next(x for x in verified['artifacts'] if x['name'] == 'image-metadata')
    raw = subprocess.check_output(['gh', 'api', meta['download_path'], '-H', 'Accept: application/octet-stream'], timeout=120, pass_fds=(9,))
    assert len(raw) == meta['size'] and 'sha256:' + hashlib.sha256(raw).hexdigest() == meta['digest']
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        accepted = json.loads(archive.read('image-metadata.json'))
    assert accepted['revision'] == sha and accepted['image'] + '@' + accepted['digest'] == image
    command(['docker', 'pull', image], timeout=300)
    metadata = json.loads(command(['docker', 'image', 'inspect', image]))[0]
    assert metadata['Config']['Labels']['org.opencontainers.image.revision'] == sha
    assert image in metadata['RepoDigests']
    old = topology()
    assert command(['git', 'rev-parse', 'HEAD']).strip() == sha, 'Canonical source differs'
    actual_data = next(m for m in old['Mounts'] if m['Destination'] == '/app/data')
    actual_quality = next(m for m in old['Mounts'] if m['Destination'] == '/var/lib/grok2api-quality-guard')
    assert actual_data['Source'] == str(DATA) and actual_quality['Source'] == str(QUALITY)
    health()
    authenticated_smoke()
    STATE.mkdir(mode=0o700, parents=True, exist_ok=True)
    record = {'sourceSha': sha, 'image': image, 'oldImage': old['Config']['Image'], 'state': 'preparing', 'startedAt': time.time(), 'ci': verified['builder']}
    previous = STATE / 'transaction.json'
    if previous.exists():
        pending = json.loads(previous.read_text())
        assert pending.get('state') not in ('preparing', 'stop_requested', 'stopped', 'replacing_database', 'switching', 'recovery_required'), 'Existing transaction needs reconciliation'
    envfile = ROOT / '.env'
    oldenv = envfile.read_bytes()
    prepared = STATE / ('recovery-' + sha)
    assert not prepared.exists(), 'Existing recovery intent requires reconciliation'
    prepared.mkdir(mode=0o700)
    atomic(STATE / 'transaction.json', record)
    started = False
    stopped = False
    backup_ready = False
    db_replaced = False
    try:
        # Keep schema job directories until the container cleanup is confirmed.
        preliminary = Path(tempfile.mkdtemp(prefix='schema-proof-', dir=STATE))
        migration(image, DATA / 'backend.db', preliminary)
        shutil.rmtree(preliminary)
        stopped = True
        record['state'] = 'stop_requested'
        atomic(STATE / 'transaction.json', record)
        command(COMPOSE + ['stop', '--timeout', '30', 'grok2api'])
        assert not inspect('grok2api')['State']['Running'], 'Old writer did not stop'
        record['state'] = 'stopped'
        atomic(STATE / 'transaction.json', record)
        snapshot(DATA / 'backend.db', prepared / 'backend.db')
        backup_ready = True
        for filename in ('config.yaml', '.env', 'docker-compose.yml'):
            shutil.copy2(ROOT / filename, prepared / filename)
        shutil.copytree(QUALITY, prepared / 'quality-state')
        temporary = tempfile.mkdtemp(prefix='schema-final-', dir=STATE)
        db, before, after = migration(image, prepared / 'backend.db', Path(temporary))
        record['state'] = 'replacing_database'
        atomic(STATE / 'transaction.json', record)
        snapshot(db, DATA / 'backend.next.db')
        os.chown(DATA / 'backend.next.db', 10001, 10001)
        os.chmod(DATA / 'backend.next.db', 0o600)
        os.replace(DATA / 'backend.next.db', DATA / 'backend.db')
        db_replaced = True
        for suffix in ('-wal', '-shm'):
            (DATA / ('backend.db' + suffix)).unlink(missing_ok=True)
        fd = os.open(DATA, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
        shutil.rmtree(temporary)
        atomic(prepared / 'manifest.json', {'sha': sha, 'oldFields': before, 'migratedFields': after, 'files': {str(p.relative_to(prepared)): hashlib.sha256(p.read_bytes()).hexdigest() for p in prepared.rglob('*') if p.is_file()}})
        text = oldenv.decode()
        assert len(re.findall(r'^GROK2API_IMAGE=.*$', text, re.M)) == 1
        atomic(envfile, re.sub(r'^GROK2API_IMAGE=.*$', 'GROK2API_IMAGE=' + image, text, flags=re.M).encode())
        record.update(state='switching', recovery=str(prepared), schemaPreserved=True)
        atomic(STATE / 'transaction.json', record)
        started = True  # Once up is invoked, upstream effects may exist even if it errors.
        record['startRequested'] = True
        atomic(STATE / 'transaction.json', record)
        command(COMPOSE + ['up', '-d', '--no-build', '--pull', 'never', '--wait', '--wait-timeout', '90', 'grok2api'], timeout=150)
        authenticated_smoke()
        for _ in range(36):
            health()
            current = topology()
            assert current['Image'] == metadata['Id']
            time.sleep(5)
        record.update(state='success', completedAt=time.time(), schemaIdempotent=True, oldColumnHashesPreserved=True)
        atomic(STATE / 'transaction.json', record)
        atomic(STATE / 'deployed.json', record)
        print(json.dumps({'state': 'deployed', 'sourceSha': sha, 'schemaPreserved': True, 'schemaIdempotent': True}))
    except BaseException as error:
        record.update(state='failed', errorType=type(error).__name__, newWriterMayHaveRun=started)
        atomic(STATE / 'transaction.json', record)
        if stopped and not started:
            # No candidate writer was started: restoring the old DB is safe.
            if backup_ready and db_replaced:
                snapshot(prepared / 'backend.db', DATA / 'backend.rollback.db')
                os.chown(DATA / 'backend.rollback.db', 10001, 10001)
                os.chmod(DATA / 'backend.rollback.db', 0o600)
                os.replace(DATA / 'backend.rollback.db', DATA / 'backend.db')
                for suffix in ('-wal', '-shm'):
                    (DATA / ('backend.db' + suffix)).unlink(missing_ok=True)
            atomic(envfile, oldenv)
            command(COMPOSE + ['up', '-d', '--no-build', '--pull', 'never', '--wait', '--wait-timeout', '90', 'grok2api'], timeout=150)
            health()
        elif started:
            # New credentials or business receipts may exist. Never overwrite
            # them or start an unverified old writer against the new schema.
            record['state'] = 'recovery_required'
            atomic(STATE / 'transaction.json', record)
            command(COMPOSE + ['stop', '--timeout', '30', 'grok2api'])
            snapshot(DATA / 'backend.db', prepared / 'candidate-post-start.db')
        raise


if __name__ == '__main__':
    main()
