"""Script to spawn the cluster workload for testing."""

import argparse
import multiprocessing
import logging
import time
import sys

from entroq.json import EntroQJSON
from entroq.pg import EntroQ as EntroQPostgres
from entroq import TaskData

from mr import MapWorker, ReduceWorker
from chaos import ChaosWorker

logging.basicConfig(level=logging.INFO, format="%(processName)s | %(levelname)s: %(message)s")

def get_client(backend: str):
    if backend == 'pg':
        return EntroQPostgres(connstr="host=localhost port=5432 dbname=entroq user=entroq password=entroq")
    elif backend == 'json':
        return EntroQJSON('http://localhost:9100') # Port 9100 handles JSON/Metrics in eqpg serve
    raise ValueError(f"Unknown backend {backend}")

def wordcount_map(key: bytes, value: bytes, emit):
    text = value.decode('utf-8')
    for word in text.split():
        word = word.strip().lower()
        if word:
            emit(word.encode('utf-8'), b"1")

def wordcount_reduce(key: bytes, values):
    total = sum(int(v.decode('utf-8')) for v in values)
    return str(total).encode('utf-8')

def run_mapper(backend: str, in_q: str, out_prefix: str, shards: int):
    client = get_client(backend)
    w = MapWorker(client, in_q, out_prefix, wordcount_map, shards)
    w.work()

def run_reducer(backend: str, map_empty_q: str, in_q: str, out_q: str):
    client = get_client(backend)
    w = ReduceWorker(client, map_empty_q, in_q, out_q, wordcount_reduce)
    w.work()

def run_chaos(backend: str, queues):
    client = get_client(backend)
    w = ChaosWorker(client)
    w.work(queues)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--backend', choices=['pg', 'json'], default='json')
    parser.add_argument('--mappers', type=int, default=3)
    parser.add_argument('--reducers', type=int, default=2)
    parser.add_argument('--chaos', type=int, default=1)
    args = parser.parse_args()

    client = get_client(args.backend)
    
    prefix = '/test/mr'
    q_map_in = f"{prefix}/map/input"
    q_reduce_in = f"{prefix}/reduce/input"
    q_reduce_out = f"{prefix}/reduce/output"

    # Seed data
    logging.info("Seeding data...")
    seed_text = "the quick brown fox jumps over the lazy dog " * 100 + "dog dog dog"
    # split into a few tasks
    inserts = []
    chunk_size = 50
    words = seed_text.split()
    for i in range(0, len(words), chunk_size):
        chunk = " ".join(words[i:i+chunk_size])
        val = f'{{"key": "chunk{i}", "value": "{chunk}"}}'
        inserts.append(TaskData(queue=q_map_in, value=val.encode('utf-8')))
    client.modify(inserts=inserts)

    map_procs = []
    for i in range(args.mappers):
        p = multiprocessing.Process(target=run_mapper, args=(args.backend, q_map_in, q_reduce_in, args.reducers), name=f"Mapper-{i}")
        map_procs.append(p)
    
    reduce_procs = []
    for i in range(args.reducers):
        p = multiprocessing.Process(target=run_reducer, args=(args.backend, q_map_in, f"{q_reduce_in}/{i}", q_reduce_out), name=f"Reducer-{i}")
        reduce_procs.append(p)
    
    chaos_procs = []
    for i in range(args.chaos):
        p = multiprocessing.Process(target=run_chaos, args=(args.backend, [q_map_in]), name=f"Chaos-{i}")
        chaos_procs.append(p)

    procs = map_procs + reduce_procs + chaos_procs

    logging.info("Starting cluster...")
    for p in procs:
        p.start()

    # Monitor
    try:
        while True:
            qs = client.queues(prefix=prefix)
            map_sz = sum(q['num_tasks'] for q in qs if q['name'].startswith(q_map_in))
            red_sz = sum(q['num_tasks'] for q in qs if q['name'].startswith(q_reduce_in))

            map_alive = sum(1 for p in map_procs if p.is_alive())
            red_alive = sum(1 for p in reduce_procs if p.is_alive())
            chaos_alive = sum(1 for p in chaos_procs if p.is_alive())

            logging.info("MainProcess | INFO: Remaining - Map: %d (Mappers: %d alive), Reduce Input: %d (Reducers: %d alive), Chaos: %d alive", 
                         map_sz, map_alive, red_sz, red_alive, chaos_alive)

            if map_sz == 0 and red_sz == 0:
                logging.info("MainProcess | INFO: All queues empty, MapReduce finished!")
                break
            
            if map_sz > 0 and map_alive == 0:
                logging.error("MainProcess | ERROR: Mappers have all died with %d tasks remaining!", map_sz)
            
            time.sleep(10)
    except KeyboardInterrupt:
        pass
    finally:
        for p in procs:
            p.terminate()

if __name__ == '__main__':
    main()
