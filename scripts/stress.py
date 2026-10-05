# Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
# Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
# Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)
"""Stress and failure measurements against the 3-site test bench (scripts\\up.ps1).

    python scripts/stress.py <test> [...]
    tests: throughput latency bigtx wal altersystem pwleak newtable joinflood gossip removed schema bulkconflict static
"""
import json, subprocess, sys, time, urllib.request, statistics, threading, os

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DASH = {'host': 7420, 'local': 7421, 'branch': 7422}
PEER = {'host': 7440, 'local': 7441, 'branch': 7442}
ADMIN = 'admin'


def sh(args, inp=None, check=True):
    r = subprocess.run(args, input=inp, capture_output=True, text=True, encoding='utf-8', cwd=ROOT)
    if check and r.returncode != 0:
        raise RuntimeError(f"{' '.join(args)}: {r.stderr.strip()[:400]}")
    return r.stdout.strip()


def sql(node, q):
    return sh(['docker', 'compose', '--profile', 'branch', 'exec', '-T', f'pg-{node}',
               'psql', '-U', 'postgres', '-d', 'app', '-v', 'ON_ERROR_STOP=1', '-qtA', '-f', '-'], q)


def status(node):
    with urllib.request.urlopen(f'http://127.0.0.1:{DASH[node]}/status', timeout=5) as r:
        return json.load(r)


def container(svc):
    return sh(['docker', 'compose', '--profile', 'branch', 'ps', '-q', svc])


def cut(node):
    sh(['docker', 'network', 'disconnect', 'conduit_wan', container(f'conduit-{node}')])


def heal(node):
    sh(['docker', 'network', 'connect', '--alias', f'conduit-{node}', 'conduit_wan', container(f'conduit-{node}')])


def mem(svc):
    return sh(['docker', 'stats', '--no-stream', '--format', '{{.MemUsage}}', container(svc)])


def restarts(svc):
    out = sh(['docker', 'inspect', '-f', '{{.RestartCount}} oom={{.State.OOMKilled}}', container(svc)])
    return out


def wait_until(fn, timeout, every=0.2):
    t0 = time.time()
    while time.time() - t0 < timeout:
        if fn():
            return time.time() - t0
        time.sleep(every)
    return None


def table_on_all(ddl, nodes=('host', 'local', 'branch')):
    for n in nodes:
        sql(n, ddl)


def log(*a):
    print(*a, flush=True)


# ------------------------------------------------------------------ tests

def t_throughput():
    """5,000 one-row transactions on host: commit rate and how long local takes to catch up."""
    table_on_all('DROP TABLE IF EXISTS bench_tp; CREATE TABLE bench_tp (id BIGSERIAL PRIMARY KEY, v TEXT, at TIMESTAMPTZ DEFAULT now());')
    time.sleep(2)
    n = 5000
    script = ''.join(f"INSERT INTO bench_tp (v) VALUES ('row {i}');\n" for i in range(n))
    t0 = time.time()
    sql('host', script)
    t_commit = time.time() - t0
    lag = wait_until(lambda: sql('local', 'SELECT count(*) FROM bench_tp') == str(n), 300, 0.5)
    total = time.time() - t0
    log(f'  host committed {n} txs in {t_commit:.1f}s ({n / t_commit:.0f} tx/s)')
    if lag is None:
        log(f'  local did NOT catch up within 300s (has {sql("local", "SELECT count(*) FROM bench_tp")})')
    else:
        log(f'  local had all {n} after {total:.1f}s from start -> sync rate ~{n / total:.0f} tx/s, '
            f'extra lag after host finished: {max(0, total - t_commit):.1f}s')
    log(f'  conduit-host mem {mem("conduit-host")} | conduit-local mem {mem("conduit-local")}')


def t_latency():
    """Time from commit on host to the row landing on local, measured inside Postgres (300 samples)."""
    sql('host', 'DROP TABLE IF EXISTS bench_lat; CREATE TABLE bench_lat (id BIGSERIAL PRIMARY KEY, sent TIMESTAMPTZ, arrived TIMESTAMPTZ);')
    sql('local', """DROP TABLE IF EXISTS bench_lat; CREATE TABLE bench_lat (id BIGSERIAL PRIMARY KEY, sent TIMESTAMPTZ, arrived TIMESTAMPTZ);
        CREATE OR REPLACE FUNCTION stamp_arrival() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN NEW.arrived := clock_timestamp(); RETURN NEW; END $$;
        CREATE TRIGGER stamp BEFORE INSERT ON bench_lat FOR EACH ROW EXECUTE FUNCTION stamp_arrival();
        ALTER TABLE bench_lat ENABLE ALWAYS TRIGGER stamp;""")
    time.sleep(2)
    # one row per transaction, 20 ms apart, each stamped with its own commit-time clock
    script = "INSERT INTO bench_lat (sent) VALUES (clock_timestamp()); SELECT pg_sleep(0.02);\n" * 300
    sql('host', script)
    wait_until(lambda: sql('local', 'SELECT count(*) FROM bench_lat') == '300', 60, 0.5)
    r = sql('local', """SELECT round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ms)::numeric, 1),
        round(percentile_cont(0.95) WITHIN GROUP (ORDER BY ms)::numeric, 1), round(max(ms)::numeric, 1)
        FROM (SELECT extract(epoch FROM arrived - sent) * 1000 AS ms FROM bench_lat) x""").split('|')
    log(f'  host commit -> row on local: median {r[0]} ms, p95 {r[1]} ms, max {r[2]} ms (300 samples)')


def t_bigtx():
    """One big transaction of N rows: does it reach local? payload size, memory, restarts."""
    table_on_all('DROP TABLE IF EXISTS bench_big; CREATE TABLE bench_big (id BIGINT PRIMARY KEY, pad TEXT);')
    time.sleep(2)
    for n in (10_000, 50_000, 100_000, 200_000):
        sql('host', 'TRUNCATE bench_big')  # TRUNCATE is not replicated; do it everywhere
        sql('local', 'TRUNCATE bench_big')
        sql('branch', 'TRUNCATE bench_big')
        before = restarts('conduit-host'), restarts('conduit-local')
        t0 = time.time()
        sql('host', f"INSERT INTO bench_big SELECT g, repeat('x', 200) FROM generate_series(1, {n}) g")
        size = None
        def queued():
            nonlocal size
            size = sql('host', 'SELECT coalesce(max(pg_column_size(payload)), 0) FROM conduit.outbox')
            return size not in ('', '0')
        wait_until(queued, 60, 0.5)
        done = wait_until(lambda: sql('local', 'SELECT count(*) FROM bench_big') == str(n), 120, 1)
        after = restarts('conduit-host'), restarts('conduit-local')
        st = [p for p in status('host').get('peers', []) if p['id'] == 'local']
        err = st[0].get('last_error', '')[:110] if st else ''
        mb = int(size or 0) / 1e6
        if done is not None:
            log(f'  {n:>7} rows: OK, reached local in {time.time() - t0:.1f}s | outbox payload {mb:.1f} MB (compressed jsonb)'
                f' | restarts host/local {before} -> {after}')
        else:
            log(f'  {n:>7} rows: STUCK after 120s (local has {sql("local", "SELECT count(*) FROM bench_big")})'
                f' | payload {mb:.1f} MB | restarts {before} -> {after}\n           last error: {err}')
            return False
    return True


def t_wal():
    """Stop conduit-local (its capture) while local keeps writing: how much WAL Postgres must keep."""
    q = ("SELECT pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)), "
         "current_setting('max_slot_wal_keep_size') FROM pg_replication_slots WHERE slot_name = 'conduit_slot'")
    table_on_all('DROP TABLE IF EXISTS bench_wal; CREATE TABLE bench_wal (id BIGSERIAL PRIMARY KEY, pad TEXT);')
    time.sleep(2)
    log(f'  before: retained WAL / max_slot_wal_keep_size = {sql("local", q)}')
    sh(['docker', 'compose', 'stop', 'conduit-local'])
    for k in range(5):
        sql('local', "INSERT INTO bench_wal (pad) SELECT repeat('w', 500) FROM generate_series(1, 100000)")
        log(f'  conduit-local stopped, local wrote {(k + 1) * 100_000:>7} rows -> retained WAL {sql("local", q).split("|")[0]}')
    sh(['docker', 'compose', 'start', 'conduit-local'])
    d = wait_until(lambda: sql('host', 'SELECT count(*) FROM bench_wal') == '500000', 300, 2)
    time.sleep(12)
    log(f'  after restart: host caught up in {d if d is None else round(d, 1)}s, retained WAL now {sql("local", q).split("|")[0]}')
    log('  -> retained WAL grows with every write while Conduit is down; with max_slot_wal_keep_size = -1 there is no cap')


def t_altersystem():
    """Does Conduit wait for an admin's permission before changing Postgres settings?"""
    out = sh(['docker', 'compose', '--profile', 'branch', 'exec', '-T', 'pg-branch', 'sh', '-c',
              'cat "$PGDATA/postgresql.auto.conf"'])
    logs = sh(['docker', 'compose', '--profile', 'branch', 'logs', 'conduit-branch'])
    lines = logs.splitlines()
    asked = next((i for i, l in enumerate(lines) if 'phase=needs_config' in l), None)
    allowed = next((i for i, l in enumerate(lines) if 'admin allowed changing Postgres settings' in l), None)
    changed = next((i for i, l in enumerate(lines) if 'changed Postgres settings' in l), None)
    log(f'  waited for permission: {"yes" if asked is not None else "no"} | '
        f'changed only after the admin allowed it: {"yes" if None not in (allowed, changed) and allowed < changed else "NO"}')
    log('  settings now in postgresql.auto.conf: ' + ', '.join(l for l in out.splitlines() if not l.startswith('#')))


def t_pwleak():
    """Is the database password visible in the process list while pg_dump runs?"""
    creds = 'Conduit ' + sql('local', "SELECT id || ':' || token FROM conduit.node")
    found = []
    def watch():
        end = time.time() + 6
        while time.time() < end:
            out = sh(['docker', 'compose', 'exec', '-T', 'conduit-host', 'ps', '-o', 'args'], check=False)
            found.extend(l for l in out.splitlines() if 'pg_dump' in l)
    th = threading.Thread(target=watch); th.start()
    time.sleep(0.5)
    for _ in range(15):
        sh(['curl.exe', '-sk', '-o', 'NUL', '-H', f'Authorization: {creds}', f'https://127.0.0.1:{PEER["host"]}/v1/schema'], check=False)
    th.join()
    if found:
        log(f'  seen in "ps" inside conduit-host ({len(found)} times), e.g.:')
        log('     ' + sorted(set(found))[0][:160])
    else:
        log('  pg_dump was not caught in ps (it may have been too fast)')


def t_newtable():
    """Table created after Conduit started: do two sites hand out the same id?"""
    table_on_all('DROP TABLE IF EXISTS bench_new; CREATE TABLE bench_new (id SERIAL PRIMARY KEY, who TEXT);', ('host', 'local'))
    time.sleep(3)
    cut('local')
    sql('host', "INSERT INTO bench_new (who) VALUES ('made on host')")
    time.sleep(1)
    sql('local', "INSERT INTO bench_new (who) VALUES ('made on local')")
    log(f"  while cut: host has {sql('host', 'SELECT string_agg(id || ' + chr(39) + '=' + chr(39) + ' || who, ' + chr(39) + ', ' + chr(39) + ') FROM bench_new')}")
    log(f"             local has {sql('local', 'SELECT string_agg(id || ' + chr(39) + '=' + chr(39) + ' || who, ' + chr(39) + ', ' + chr(39) + ') FROM bench_new')}")
    heal('local')
    time.sleep(15)
    for n in ('host', 'local'):
        log(f"  after heal, {n:<5}: {sql(n, 'SELECT count(*) || ' + chr(39) + ' rows: ' + chr(39) + ' || string_agg(id || ' + chr(39) + '=' + chr(39) + ' || who, ' + chr(39) + ', ' + chr(39) + ') FROM bench_new')}")
    log(f"  conflicts recorded: {sql('local', 'SELECT count(*) FROM conduit.conflicts WHERE table_name = ' + chr(39) + 'bench_new' + chr(39))}")


def t_joinflood():
    """Can anyone fill the join-request queue so a real site cannot ask to join?"""
    body = '{"node_id":"fake%d","url":"https://x:1","code":"000 000","secret":"0123456789abcdef0123"}'
    codes = []
    for i in range(22):
        r = subprocess.run(['curl.exe', '-sk', '-o', 'NUL', '-w', '%{http_code}', '-X', 'POST', '-H', 'Content-Type: application/json',
                            '--data-binary', body % i, f'https://127.0.0.1:{PEER["host"]}/v1/join-requests'],
                           capture_output=True, text=True)
        codes.append(r.stdout)
    log(f'  22 unauthenticated join requests -> HTTP codes: {" ".join(codes)}')
    log(f'  host dashboard now lists {len(status("host").get("join_requests", []))} pending requests (all fake)')


def t_gossip():
    """A compromised member forges another site's address + fingerprint in its member list."""
    real = sql('branch', "SELECT url || ' ' || left(cert_fp, 12) FROM conduit.members WHERE id = 'host'")
    log(f'  branch sees host as: {real}')
    sql('local', "UPDATE conduit.members SET url = 'https://evil.example:7443', cert_fp = repeat('0', 64), "
                 "updated_at = now() + interval '1 hour' WHERE id = 'host'")
    log('  local (pretend compromised) rewrote host\'s record; waiting for gossip ...')
    time.sleep(25)
    for n in ('branch', 'host'):
        log(f"  {n} now sees host as: {sql(n, 'SELECT url || ' + chr(39) + ' ' + chr(39) + ' || left(cert_fp, 12) FROM conduit.members WHERE id = ' + chr(39) + 'host' + chr(39))}")
    p = [x for x in status('branch').get('peers', []) if x['id'] == 'host']
    log(f"  branch -> host sender: {(p[0].get('last_error') or 'ok')[:120] if p else 'no sender'}")


def t_removed():
    """A removed site keeps capturing: does its outbox keep growing?"""
    code = sh(['curl.exe', '-s', '-o', 'NUL', '-w', '%{http_code}', '-X', 'POST', '-H', f'X-Admin-Password: {ADMIN}',
               f'http://127.0.0.1:{DASH["host"]}/v1/admin/members/branch/remove'])
    log(f'  removed branch (HTTP {code}); waiting for it to notice ...')
    wait_until(lambda: status('branch').get('removed') is True, 60, 2)
    q = 'SELECT count(*) || ' + chr(39) + ' rows, ' + chr(39) + ' || pg_size_pretty(pg_total_relation_size(' + chr(39) + 'conduit.outbox' + chr(39) + ')) FROM conduit.outbox'
    log(f'  branch outbox before: {sql("branch", q)}')
    sql('branch', 'CREATE TABLE IF NOT EXISTS bench_rm (id BIGSERIAL PRIMARY KEY, pad TEXT)')
    for k in range(3):
        sql('branch', ''.join("INSERT INTO bench_rm (pad) VALUES (repeat('r', 200));\n" for _ in range(1000)))
        time.sleep(3)
        log(f'  branch wrote {(k + 1) * 1000} more txs -> outbox {sql("branch", q)}')
    sql('branch', 'SELECT 1')
    slot = sql('branch', "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'conduit_slot'")
    log(f'  branch replication slot still exists: {"yes" if slot != "0" else "no"}')


def t_schema():
    """Table differs on one site: is the rest of the queue still delivered, and can the skipped rows be replayed?"""
    sql('host', 'DROP TABLE IF EXISTS bench_schema; CREATE TABLE bench_schema (id BIGINT PRIMARY KEY, extra TEXT)')
    sql('local', 'DROP TABLE IF EXISTS bench_schema; CREATE TABLE bench_schema (id BIGINT PRIMARY KEY, extra TEXT)')
    sql('branch', 'DROP TABLE IF EXISTS bench_schema; CREATE TABLE bench_schema (id BIGINT PRIMARY KEY)')  # no "extra"
    time.sleep(2)
    sql('host', "INSERT INTO bench_schema SELECT g, 'x' FROM generate_series(1, 5) g")
    sql('host', "INSERT INTO customers (code, name) VALUES ('AFTER-SCHEMA', 'queue keeps moving')")
    ok = wait_until(lambda: sql('branch', "SELECT count(*) FROM customers WHERE code = 'AFTER-SCHEMA'") == '1', 60, 1)
    log(f'  later change for another table reached branch: {"yes" if ok is not None else "NO (queue blocked)"}')
    log(f"  skipped on branch: {sql('branch', 'SELECT count(*) FROM conduit.conflicts WHERE kind = ' + chr(39) + 'schema_mismatch' + chr(39))} changes, "
        f"branch has {sql('branch', 'SELECT count(*) FROM bench_schema')} rows")
    sql('branch', 'ALTER TABLE bench_schema ADD COLUMN extra TEXT')
    out = sh(['curl.exe', '-s', '-X', 'POST', '-H', f'X-Admin-Password: {ADMIN}', f'http://127.0.0.1:{DASH["branch"]}/v1/admin/replay'])
    log(f'  after fixing the table + replay ({out}): branch has {sql("branch", "SELECT count(*) FROM bench_schema")} of 5 rows')


def t_bulkconflict():
    """Both sites update the same 500 rows in one transaction each while cut off: does the later one win everywhere?"""
    table_on_all('DROP TABLE IF EXISTS bench_bulk; CREATE TABLE bench_bulk (id BIGINT PRIMARY KEY, v TEXT);')
    time.sleep(2)
    sql('host', "INSERT INTO bench_bulk SELECT g, 'start' FROM generate_series(1, 500) g")
    wait_until(lambda: sql('local', 'SELECT count(*) FROM bench_bulk') == '500', 60, 1)
    before = sql('local', "SELECT count(*) FROM conduit.conflicts WHERE table_name = 'bench_bulk'")
    cut('local')
    sql('host', "UPDATE bench_bulk SET v = 'host (earlier)'")
    time.sleep(1)
    sql('local', "UPDATE bench_bulk SET v = 'local (later)'")
    heal('local')
    ok = wait_until(lambda: all(sql(n, "SELECT count(*) FROM bench_bulk WHERE v = 'local (later)'") == '500' for n in ('host', 'local', 'branch')), 120, 2)
    after = sql('local', "SELECT count(*) FROM conduit.conflicts WHERE table_name = 'bench_bulk'")
    log(f'  later bulk update won on all 3 sites: {"yes" if ok is not None else "NO"} | '
        f'conflicts recorded on local: {int(after) - int(before)} (expected 500: host\'s older update skipped row by row)')


def t_static():
    """Unit tests, module path and exposed ports."""
    out = sh(['docker', 'run', '--rm', '-v', f'{ROOT}:/src', '-w', '/src', 'golang:1.25-alpine', 'sh', '-c',
              'go test -list . ./... 2>/dev/null | grep -c ^Test; go list ./... | wc -l; '
              'go test -list . ./... 2>/dev/null | grep -B1 -c ^ok'])
    n_tests, n_pkgs = out.split()[0], out.split()[1]
    with_tests = sh(['git', '-C', ROOT, 'ls-files', '*_test.go']).splitlines()
    log(f'  unit tests: {n_tests} test functions in {len({os.path.dirname(f) for f in with_tests})} of {n_pkgs} packages')
    log(f'  module path: {sh(["git", "-C", ROOT, "show", "HEAD:go.mod"]).splitlines()[0]}  (repo is github.com/MorphEdit/Conduit)')
    log(f'  image exposes: {sh(["docker", "image", "inspect", "-f", "{{json .Config.ExposedPorts}}", "conduit:dev"])}')


TESTS = {k[2:]: v for k, v in globals().items() if k.startswith('t_')}

if __name__ == '__main__':
    for name in sys.argv[1:] or ['static']:
        log(f'\n== {name}: {TESTS[name].__doc__}')
        TESTS[name]()
