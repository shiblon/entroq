"""Spawn a MapReduce cluster workload for testing.

Each worker runs in its own process (multiprocessing 'spawn', so child processes
start fresh rather than forking the parent's asyncio state) and drives the async
client with asyncio.run.

Requires an EntroQ service (for example, `eqmem serve` or `eqpg serve`) on
http://localhost:9100.

A short --lease exercises the worker's renewal loop: a reduce that outlasts its
lease has to be renewed, along with the doc map it claimed, or the work is lost.
The service clamps a claim up to its own lease floor, so pass a matching
--claim_lease_floor to the server to make a short lease stick.
"""

import argparse
import asyncio
import logging
import multiprocessing
import uuid
from collections import Counter

from entroq.json import EntroQJSON

import mr
from chaos import ChaosWorker

logging.basicConfig(level=logging.INFO, format="%(processName)s | %(levelname)s: %(message)s")
logging.getLogger("httpx").setLevel(logging.WARNING)

SEED_TEXT = "the quick brown fox jumps over the lazy dog " * 100 + "dog dog dog"
WORDS_PER_SPLIT = 50


def get_client():
    return EntroQJSON("http://localhost:9100")


def wordcount_map(key: str, value: str, emit):
    for word in value.split():
        word = word.strip().lower()
        if word:
            emit(word, "1")


def wordcount_reduce(key: str, values) -> str:
    return str(sum(int(v) for v in values))


# Process entry points: each builds its own client and runs the async loop.

def run_mapper(map_q, lease):
    async def main():
        async with get_client() as eq:
            await mr.run_mapper(eq, map_q, wordcount_map, claim_duration_s=lease)
    asyncio.run(main())


def run_reducer(reduce_q, lease, delay):
    async def main():
        async with get_client() as eq:
            await mr.run_reducer(eq, reduce_q, wordcount_reduce,
                                 claim_duration_s=lease, reduce_delay_s=delay)
    asyncio.run(main())


def run_chaos(queues, hold):
    async def main():
        async with get_client() as eq:
            await ChaosWorker(eq).work(queues, hold_s=hold)
    asyncio.run(main())


def splits_of(text: str, words_per_split: int) -> list[str]:
    words = text.split()
    return [" ".join(words[i:i + words_per_split])
            for i in range(0, len(words), words_per_split)]


async def drain(eq, queue: str, what: str) -> None:
    """Wait until queue holds no tasks."""
    while True:
        qs = await eq.queues(exact=[queue])
        remaining = qs[0].get("num_tasks", 0) if qs else 0
        if remaining == 0:
            logging.info("%s drained", what)
            return
        logging.info("%s: %d remaining", what, remaining)
        await asyncio.sleep(2)


def check(got: dict) -> int:
    """Compare the run's output against what the seed text implies."""
    want = Counter(SEED_TEXT.split())
    counts = {k: int(v) for k, v in got.items()}
    bad = {k: (want.get(k), counts.get(k)) for k in set(want) | set(counts)
           if want.get(k) != counts.get(k)}
    logging.info("distinct words: want %d, got %d", len(want), len(counts))
    logging.info("total words:    want %d, got %d",
                 sum(want.values()), sum(counts.values()))
    if bad:
        logging.error("MISMATCHED COUNTS (want, got): %s", dict(sorted(bad.items())))
        return 1
    logging.info("counts match exactly: %s", dict(sorted(counts.items())))
    return 0


async def orchestrate(args, ns, map_q, reduce_q, procs):
    async with get_client() as eq:
        splits = splits_of(SEED_TEXT, WORDS_PER_SPLIT)
        n = await mr.seed(eq, ns, map_q, splits, args.reducers)
        logging.info("seeded %d splits into %s (namespace %s)", n, map_q, ns)

        for p in procs:
            p.start()

        # The map stage has to finish before any reducer starts: a reducer that
        # ran while mappers were still emitting would reduce a partial
        # partition and record it as final.
        await drain(eq, map_q, "map")
        await mr.seed_reduces(eq, ns, reduce_q, args.reducers)
        logging.info("seeded %d reduce tasks into %s", args.reducers, reduce_q)

        await drain(eq, reduce_q, "reduce")
        return check(await mr.results(eq, ns, args.reducers))


def main():
    multiprocessing.set_start_method("spawn", force=True)
    parser = argparse.ArgumentParser()
    parser.add_argument("--mappers", type=int, default=3)
    parser.add_argument("--reducers", type=int, default=4)
    parser.add_argument("--chaos", type=int, default=1)
    parser.add_argument("--lease", type=float, default=2.0,
                        help="Claim lease in seconds; the worker renews at two thirds of it.")
    parser.add_argument("--reduce-delay", type=float, default=0.0,
                        help="Seconds to stall each reduce, to force lease renewal.")
    parser.add_argument("--chaos-hold", type=float, default=2.0,
                        help="Seconds the chaos worker holds a task before dropping it.")
    args = parser.parse_args()

    run = uuid.uuid4().hex[:8]
    prefix = f"/test/mr/{run}"
    ns, map_q, reduce_q = f"{prefix}/docs", f"{prefix}/map", f"{prefix}/reduce"

    procs = (
        [multiprocessing.Process(target=run_mapper,
                                 args=(map_q, args.lease),
                                 name=f"Mapper-{i}") for i in range(args.mappers)]
        + [multiprocessing.Process(target=run_reducer,
                                   args=(reduce_q, args.lease, args.reduce_delay),
                                   name=f"Reducer-{i}") for i in range(args.reducers)]
        + [multiprocessing.Process(target=run_chaos,
                                   args=([map_q, reduce_q], args.chaos_hold),
                                   name=f"Chaos-{i}") for i in range(args.chaos)]
    )

    status = 1
    try:
        status = asyncio.run(orchestrate(args, ns, map_q, reduce_q, procs))
    except KeyboardInterrupt:
        pass
    finally:
        for p in procs:
            p.terminate()
        for p in procs:
            p.join(timeout=5)
    raise SystemExit(status)


if __name__ == "__main__":
    main()
