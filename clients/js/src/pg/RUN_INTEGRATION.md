# EntroQ PostgreSQL TS Client Integration Test

To run the integration tests for the direct-to-PostgreSQL client, follow these steps:

## 1. Start the Database
Use the project's docker-compose to spin up a pre-configured PostgreSQL instance.

```bash
docker-compose up -d postgres
```

## 2. Initialize the Schema
Build the Go utility and use it to initialize or update the database schema.

```bash
# From the project root
go build -o eqpg ./cmd/eqpg/main.go
./eqpg schema init --dbuser=entroq --dbpwd=entroq --dbname=entroq --dbaddr=localhost:5432
```

## 3. Run the Tests
Set the environment variables to match the database configuration and run the Vitest suite.

```bash
export PGHOST=localhost
export PGPORT=5432
export PGUSER=entroq
export PGDATABASE=entroq
export PGPASSWORD=entroq

cd clients/js
./node_modules/.bin/vitest run src/pg/integration.test.ts
```

## Troubleshooting
If you see "schema version mismatch", run `./eqpg schema upgrade` to bring the database up to the version expected by the client.
