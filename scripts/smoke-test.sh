#!/bin/bash
set -e

# EntroQ Cross-Language Smoke Test
# This script verifies that Go and JS clients can both interact
# with the same EntroQ backend (using the docker-compose sandbox).

# 1. Start the sandbox
echo "--- Starting EntroQ Sandbox ---"
docker-compose up -d --build

# Cleanup on exit
trap 'docker-compose down' EXIT

# 2. Wait for EntroQ to be ready (max 30s)
echo "--- Waiting for EntroQ to be healthy ---"
for i in {1..30}; do
  if docker exec $(docker-compose ps -q entroq) /bin/eqpg schema check > /dev/null 2>&1; then
    echo "EntroQ is ready!"
    break
  fi
  sleep 1
done

# 3. Go Smoke Test
echo "--- Running Go Smoke Test ---"
go test -v -run Example_manualClaimAndRenew github.com/shiblon/entroq

# 4. JS/TS Smoke Test (Node), through an eqlink work child
echo "--- Running JS Smoke Test ---"
(cd clients/js && npm install --no-package-lock && npm run build > /dev/null 2>&1)
EQLINK_DIR=$(mktemp -d)
go build -o "$EQLINK_DIR/eqlink" ./cmd/eqlink
EQLINK="$EQLINK_DIR/eqlink" node -e "
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
                seen = w.value;
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
rm -rf "$EQLINK_DIR"

echo "--- ALL SMOKE TESTS PASSED ---"
