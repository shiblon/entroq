# EntroQ JavaScript Client

The EntroQ JavaScript client allows you to interact with an EntroQ task queue from Node.js environments. It provides a simple, high-level API for claiming, modifying, and streaming tasks.

## Installation

From this directory, install dependencies:

```bash
npm install
```

## Running Tests

The test suite is divided into unit tests (which mock the network) and integration tests (which run against a real, ephemeral EntroQ server).

### Prerequisites

For **Integration Tests**, you must have the **Go** toolchain installed. The test suite automatically builds a local `eqmemsvc` binary to serve as a back-end during the test run.

### Execute Tests

From the `clients/js` directory:

```bash
# Run all tests (unit + integration)
npm test

# Run tests with coverage reporting
npx vitest run --coverage
```

## Architecture

- **`EntroQClient`**: The low-level HTTP/JSON client. Handles the wire protocol, claimant ID generation, and RESTful transcoding via Vanguard.
- **`EntroQWorker`**: A high-level worker abstraction that handles the automatic renewal of task leases while your work is being performed.
- **Streaming**: Supports real-time task listing via HTTP Chunked Transfer Encoding (NDJSON).

## Testing Strategy

- **Unit Tests (`*.test.ts`)**: Located in `src/`. These use `vitest` mocks to verify the internal logic of the client and worker without requiring a network connection.
- **Integration Tests (`integration.test.ts`)**: These spin up a background `eqmemsvc` instance, ensuring that the client correctly handles real-world scenarios, including chunked streaming boundaries and server-side timing.
