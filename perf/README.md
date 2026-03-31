# EntroQ Performance Benchmark

## Start a tuned PostgreSQL container

```bash
mkdir -p /tmp/pgperf

docker run -d \
  -p 5432:5432 \
  -e POSTGRES_PASSWORD=password \
  -v /tmp/pgperf:/var/lib/postgresql/data \
  --shm-size=256m \
  --name pgperf \
  postgres:11 \
  -c shared_buffers=256MB \
  -c work_mem=16MB \
  -c max_connections=200 \
  -c synchronous_commit=off \
  -c checkpoint_completion_target=0.9 \
  -c wal_buffers=64MB
```

## Run the benchmark

```bash
# Install entroq_pg if not already installed.
pip install -e ../clients/py/entroq_pg

# Apply schema, load 1M tasks, benchmark with 100 workers for 60s.
python bench.py --schema --load --n 1000000 --bench --workers 100 --duration 60

# Benchmark only (data already loaded from a previous run).
python bench.py --bench --workers 100 --duration 60

# Smaller comparison run (1k tasks).
python bench.py --schema --load --n 1000 --bench --workers 10 --duration 30
```

## Tear down

```bash
docker rm -f pgperf
rm -rf /tmp/pgperf
```
