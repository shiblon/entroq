#!/bin/bash
set -e

# EntroQ Cross-Language Smoke Test
# This script verifies that Go and JS clients can both interact
# with the same EntroQ backend (using the docker-compose sandbox).

# 1. Start the sandbox
echo "--- Starting EntroQ Sandbox ---"
docker-compose up -d --build

# Local builds of the command-line tools the tests drive the sandbox with.
BIN_DIR=$(mktemp -d)
trap 'docker-compose down; rm -rf "$BIN_DIR"' EXIT
go build -o "$BIN_DIR/eqc" ./cmd/eqc
go build -o "$BIN_DIR/eqlink" ./cmd/eqlink
EQC="$BIN_DIR/eqc --svcaddr localhost:37706"

# 2. Wait for EntroQ to answer (max 60s)
echo "--- Waiting for EntroQ to be healthy ---"
for i in {1..60}; do
  if $EQC time > /dev/null 2>&1; then
    echo "EntroQ is ready!"
    break
  fi
  if [ "$i" = 60 ]; then
    echo "EntroQ did not answer within 60s" >&2
    exit 1
  fi
  sleep 1
done

# 3. Go Smoke Test: the Go client, through eqc, inserts and claims a task
echo "--- Running Go Smoke Test ---"
$EQC ins -q /smoke/go -v '"go-work"' > /dev/null
if ! $EQC claim -q /smoke/go | grep -q 'go-work'; then
  echo "Go claim did not return the inserted task" >&2
  exit 1
fi
echo "Go insert/claim success!"

# 4. JS/TS Smoke Test (Node), through an eqlink work child
echo "--- Running JS Smoke Test ---"
(cd clients/js && npm install --no-package-lock && npm run build > /dev/null 2>&1)
EQLINK="$BIN_DIR/eqlink" node -e "
const { Worker, M } = require('./clients/js/dist/index.js');
const { NodeGateway } = require('./clients/js/dist/node.js');
(async () => {
    try {
        const seeded = await fetch('http://localhost:9100/api/v0/modify', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ claimantId: 'smoke', inserts: [{ queue: '/smoke/js', value: 'js-work' }] }),
        });
        if (!seeded.ok) {
            throw new Error('seed: ' + seeded.status + ' ' + await seeded.text());
        }
        let seen;
        const gateway = new NodeGateway({ eqlink: process.env.EQLINK, entroq: 'localhost:37706', handleSignals: false });
        await gateway.run(new Worker(['/smoke/js'], {
            work: (w, ctx) => {
                seen = w.task.value;
                ctx.stop();
                return M.of(M.deleting(w.task));
            },
        }));
        if (seen !== 'js-work') {
            throw new Error('Mismatched value: ' + JSON.stringify(seen));
        }
        console.log('JS gateway worker success!');
    } catch (err) {
        console.error(err);
        process.exit(1);
    }
})();
"

echo "--- ALL SMOKE TESTS PASSED ---"
