# EntroQ

A task queue with strong competing-consumer-friendly semantics.

## Starting a PostgreSQL instance in Docker

```
docker pull postgres
docker run --name pgsql -p 5432:5432 -e POSTGRES_PASSWORD=password -d postgres
```

Now you can access it from the host on its usual port.
