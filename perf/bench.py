#!/usr/bin/env python3
"""EntroQ PostgreSQL performance benchmark.

Phases:
  --load    Disable triggers, bulk-insert N tasks via generate_series, re-enable.
  --bench   Spin up W workers claiming and deleting tasks; report latency stats.
  --compare Run two back-to-back bench runs (notify then poll-only) for comparison.
  --schema  Apply the entroq schema (idempotent) before loading.

Typical usage:

    # Fresh database: apply schema, load 1M tasks, then benchmark with 100 workers.
    python bench.py --connstr "host=localhost dbname=postgres user=postgres password=password" \\
        --schema --load --n 1000000 --bench --workers 100

    # Compare notify vs poll-only with 1k tasks and 10 workers.
    python bench.py --schema --load --n 1000 --compare --workers 10

    # Just benchmark against already-loaded data.
    python bench.py --bench --workers 100 --duration 60
"""

import argparse
import importlib.resources
import sys
import threading
import time

import psycopg

# entroq_pg must be installed (pip install -e ../clients/py/entroq_pg)
from entroq_pg import EntroQ

QUEUE = '/perf/test'


def apply_schema(connstr):
    schema = importlib.resources.files('entroq_pg').joinpath('schema.sql').read_text()
    with psycopg.connect(connstr, autocommit=True) as conn:
        conn.execute(schema)
    print('Schema applied.')


def load(connstr, n):
    print(f'Loading {n:,} tasks into {QUEUE!r}...', flush=True)
    start = time.monotonic()
    with psycopg.connect(connstr, autocommit=True) as conn:
        conn.execute('ALTER TABLE tasks DISABLE TRIGGER ALL')
        conn.execute("""
            INSERT INTO tasks (id, version, queue, at, created, modified, claimant, value, claims, attempt, err)
            SELECT
                gen_random_uuid(),
                0,
                %s,
                now(),
                now(),
                now(),
                NULL,
                ('task-' || s)::bytea,
                0, 0, ''
            FROM generate_series(1, %s) AS s
        """, (QUEUE, n))
        conn.execute('ALTER TABLE tasks ENABLE TRIGGER ALL')
    elapsed = time.monotonic() - start
    print(f'Loaded {n:,} tasks in {elapsed:.1f}s ({n/elapsed:,.0f} rows/s).')


def _run_bench(connstr, num_workers, duration_s, work_ms, poll_only, sample=None, poll_ms=50):
    """Run one bench pass; return (latencies, errors, elapsed).

    If sample is set, stop after that many claims regardless of duration_s.
    poll_ms controls how quickly workers retry after a miss (default 50ms).
    """
    latencies = []
    errors = []
    lock = threading.Lock()
    stop_event = threading.Event()

    def worker():
        eq = EntroQ(connstr)
        while not stop_event.is_set():
            try:
                t0 = time.monotonic()
                task = eq.claim(QUEUE, timeout_s=10, poll_ms=poll_ms, poll_only=poll_only)
                claim_ms = (time.monotonic() - t0) * 1000
            except TimeoutError:
                return
            except Exception as exc:
                with lock:
                    errors.append(exc)
                return

            with lock:
                latencies.append(claim_ms)
                if sample and len(latencies) >= sample:
                    stop_event.set()

            if work_ms:
                time.sleep(work_ms / 1000)

            try:
                eq.modify(deletes=[task.as_id()])
            except Exception as exc:
                with lock:
                    errors.append(exc)
                return

    threads = [threading.Thread(target=worker, daemon=True) for _ in range(num_workers)]
    start = time.monotonic()
    for t in threads:
        t.start()

    stop_event.wait(timeout=duration_s)
    stop_event.set()
    for t in threads:
        t.join(timeout=10)

    return latencies, errors, time.monotonic() - start


def _print_results(label, latencies, errors, elapsed):
    print(f'\n--- {label} ---')
    if errors:
        print(f'  ERRORS ({len(errors)}):')
        for e in errors[:5]:
            print(f'    {e}')
    if not latencies:
        print('  No tasks claimed.')
        return
    lat = sorted(latencies)
    n = len(lat)
    p = lambda pct: lat[min(int(n * pct / 100), n - 1)]
    print(f'  Tasks claimed:  {n:,}')
    print(f'  Throughput:     {n/elapsed:,.1f} tasks/s')
    print(f'  Claim latency:')
    print(f'    p50:  {p(50):7.1f} ms')
    print(f'    p90:  {p(90):7.1f} ms')
    print(f'    p99:  {p(99):7.1f} ms')
    print(f'    p99.9:{p(99.9):7.1f} ms')
    print(f'    max:  {lat[-1]:7.1f} ms')


def bench(connstr, num_workers, duration_s, work_ms, poll_only=False, poll_ms=50):
    label = 'poll-only' if poll_only else 'notify'
    print(f'Benchmarking ({label}): {num_workers} workers, {duration_s}s, {work_ms}ms work...',
          flush=True)
    latencies, errors, elapsed = _run_bench(connstr, num_workers, duration_s, work_ms, poll_only,
                                            poll_ms=poll_ms)
    _print_results(label, latencies, errors, elapsed)


def compare(connstr, num_workers, n, work_ms, poll_ms=50):
    """Load n tasks twice and run notify vs poll-only back-to-back."""
    print(f'Comparison: notify vs poll-only, {num_workers} workers, {n:,} tasks each\n')

    # Notify run.
    load(connstr, n)
    print(f'\nRunning notify pass...', flush=True)
    lat_n, err_n, el_n = _run_bench(connstr, num_workers, duration_s=300, work_ms=work_ms,
                                    poll_only=False, poll_ms=poll_ms)

    # Poll-only run.
    load(connstr, n)
    print(f'\nRunning poll-only pass...', flush=True)
    lat_p, err_p, el_p = _run_bench(connstr, num_workers, duration_s=300, work_ms=work_ms,
                                    poll_only=True, poll_ms=poll_ms)

    _print_results('notify', lat_n, err_n, el_n)
    _print_results('poll-only', lat_p, err_p, el_p)


def scale(connstr, num_workers, work_ms, sample=1000, poll_ms=50):
    """Load 1e4, 1e5, and 1e6 tasks, claiming `sample` tasks from each.

    Each run loads a fresh queue of increasing size, then claims only `sample`
    tasks from it -- enough to measure claim latency without draining the whole
    queue. The key question: does latency stay flat as the table grows, or does
    it degrade? The bucket + SKIP LOCKED design should keep it flat.
    """
    sizes = [10_000, 100_000, 1_000_000]
    print(f'Scale comparison: poll-only, {num_workers} workers, '
          f'{sample:,} claims from each of {[f"{s:,}" for s in sizes]}\n')
    results = []
    for n in sizes:
        with psycopg.connect(connstr, autocommit=True) as conn:
            conn.execute('TRUNCATE tasks')
        load(connstr, n)
        print(f'Claiming {sample:,} from {n:,}-task queue...', flush=True)
        lat, err, elapsed = _run_bench(connstr, num_workers, duration_s=300,
                                       work_ms=work_ms, poll_only=True, sample=sample,
                                       poll_ms=poll_ms)
        results.append((n, lat, err, elapsed))

    for n, lat, err, elapsed in results:
        _print_results(f'poll-only queue={n:,}', lat, err, elapsed)


def main():
    parser = argparse.ArgumentParser(description='EntroQ PG benchmark')
    parser.add_argument('--connstr',
                        default='host=localhost dbname=postgres user=postgres password=password',
                        help='psycopg connection string')
    parser.add_argument('--schema', action='store_true', help='Apply schema before loading')
    parser.add_argument('--load', action='store_true', help='Load tasks into the database')
    parser.add_argument('--n', type=int, default=1_000_000, help='Tasks to load (default 1M)')
    parser.add_argument('--bench', action='store_true', help='Run a single benchmark pass')
    parser.add_argument('--poll-only', action='store_true', help='Use poll-only claiming (no LISTEN/NOTIFY)')
    parser.add_argument('--compare', action='store_true',
                        help='Load and run notify then poll-only back-to-back (uses --n and --workers)')
    parser.add_argument('--scale', action='store_true',
                        help='Poll-only at 1e4, 1e5, 1e6 tasks to check latency vs queue depth')
    parser.add_argument('--workers', type=int, default=100, help='Worker threads (default 100)')
    parser.add_argument('--duration', type=int, default=60, help='Bench duration in seconds (default 60)')
    parser.add_argument('--work-ms', type=int, default=5, help='Simulated work per task ms (default 5)')
    parser.add_argument('--poll-ms', type=int, default=50, help='Poll interval on miss, ms (default 50)')
    args = parser.parse_args()

    if not any([args.schema, args.load, args.bench, args.compare, args.scale]):
        parser.print_help()
        sys.exit(1)

    if args.schema:
        apply_schema(args.connstr)
    if args.load:
        load(args.connstr, args.n)
    if args.bench:
        bench(args.connstr, args.workers, args.duration, args.work_ms, args.poll_only, args.poll_ms)
    if args.compare:
        compare(args.connstr, args.workers, args.n, args.work_ms, args.poll_ms)
    if args.scale:
        scale(args.connstr, args.workers, args.work_ms, poll_ms=args.poll_ms)


if __name__ == '__main__':
    main()
