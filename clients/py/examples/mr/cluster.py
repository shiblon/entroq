"""Spawn a MapReduce cluster workload for testing.

Each worker runs in its own process (multiprocessing 'spawn', so child processes
start fresh rather than forking the parent's asyncio state) and drives the async
client with asyncio.run.

Requires a running EntroQ service at the chosen backend (e.g. `eqmem serve` or
`eqpg serve` on http://localhost:9100 for the json backend).
"""

import argparse
import asyncio
import logging
import multiprocessing
import time

from entroq import Modification, TaskData
from entroq.json import EntroQJSON
from entroq.pg import EntroQ as EntroQPostgres

from mr import MapWorker, ReduceWorker
from chaos import ChaosWorker

logging.basicConfig(level=logging.INFO, format="%(processName)s | %(levelname)s: %(message)s")


def get_client(backend: str):
    if backend == "pg":
        return EntroQPostgres("host=localhost port=5432 dbname=entroq user=entroq password=entroq")
    if backend == "json":
        return EntroQJSON("http://localhost:9100")  # eqpg/eqmem serve JSON+metrics here
    raise ValueError(f"Unknown backend {backend}")


def wordcount_map(key: str, value: str, emit):
    for word in value.split():
        word = word.strip().lower()
        if word:
            emit(word, "1")


def wordcount_reduce(key: str, values) -> str:
    return str(sum(int(v) for v in values))


# Process entry points: each builds its own client and runs the async loop.

def run_mapper(backend, in_q, out_prefix, shards):
    asyncio.run(MapWorker(get_client(backend), in_q, out_prefix, wordcount_map, shards).work())


def run_reducer(backend, map_empty_q, in_q, out_q):
    asyncio.run(ReduceWorker(get_client(backend), map_empty_q, in_q, out_q, wordcount_reduce).work())


def run_chaos(backend, queues):
    asyncio.run(ChaosWorker(get_client(backend)).work(queues))


async def seed(backend: str, q_map_in: str):
    client = get_client(backend)
    seed_text = "the quick brown fox jumps over the lazy dog " * 100 + "dog dog dog"
    words = seed_text.split()
    ops = []
    for i in range(0, len(words), 50):
        chunk = " ".join(words[i:i + 50])
        ops.append(Modification.inserting(
            TaskData(queue=q_map_in, value={"key": f"chunk{i}", "value": chunk})))
    await client.modify(Modification(*ops))
    logging.info("Seeded %d map tasks into %s", len(ops), q_map_in)


async def monitor(backend, prefix, q_map_in, q_reduce_in, procs):
    client = get_client(backend)
    map_procs, reduce_procs, chaos_procs = procs
    while True:
        qs = await client.queues(prefix=prefix)
        map_sz = sum(q["num_tasks"] for q in qs if q["name"].startswith(q_map_in))
        red_sz = sum(q["num_tasks"] for q in qs if q["name"].startswith(q_reduce_in))
        logging.info("Remaining - map: %d (%d mappers alive), reduce-in: %d (%d reducers alive), chaos: %d alive",
                     map_sz, sum(p.is_alive() for p in map_procs),
                     red_sz, sum(p.is_alive() for p in reduce_procs),
                     sum(p.is_alive() for p in chaos_procs))
        if map_sz == 0 and red_sz == 0:
            logging.info("All queues empty, MapReduce finished!")
            return
        if map_sz > 0 and not any(p.is_alive() for p in map_procs):
            logging.error("Mappers all died with %d tasks remaining!", map_sz)
            return
        await asyncio.sleep(10)


def main():
    multiprocessing.set_start_method("spawn", force=True)
    parser = argparse.ArgumentParser()
    parser.add_argument("--backend", choices=["pg", "json"], default="json")
    parser.add_argument("--mappers", type=int, default=3)
    parser.add_argument("--reducers", type=int, default=2)
    parser.add_argument("--chaos", type=int, default=1)
    args = parser.parse_args()

    prefix = "/test/mr"
    q_map_in = f"{prefix}/map/input"
    q_reduce_in = f"{prefix}/reduce/input"
    q_reduce_out = f"{prefix}/reduce/output"

    logging.info("Seeding data...")
    asyncio.run(seed(args.backend, q_map_in))

    map_procs = [multiprocessing.Process(
        target=run_mapper, args=(args.backend, q_map_in, q_reduce_in, args.reducers),
        name=f"Mapper-{i}") for i in range(args.mappers)]
    reduce_procs = [multiprocessing.Process(
        target=run_reducer, args=(args.backend, q_map_in, f"{q_reduce_in}/{i}", q_reduce_out),
        name=f"Reducer-{i}") for i in range(args.reducers)]
    chaos_procs = [multiprocessing.Process(
        target=run_chaos, args=(args.backend, [q_map_in]),
        name=f"Chaos-{i}") for i in range(args.chaos)]

    procs = map_procs + reduce_procs + chaos_procs
    logging.info("Starting cluster...")
    for p in procs:
        p.start()

    try:
        asyncio.run(monitor(args.backend, prefix, q_map_in, q_reduce_in,
                            (map_procs, reduce_procs, chaos_procs)))
    except KeyboardInterrupt:
        pass
    finally:
        for p in procs:
            p.terminate()


if __name__ == "__main__":
    main()
