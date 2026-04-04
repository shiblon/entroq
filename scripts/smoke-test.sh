#!/bin/bash
set -e

# EntroQ Cross-Language Smoke Test
# This script verifies that Go, Python, and JS clients can all interact 
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

# 4. Python Smoke Test (using PYTHONPATH to point to local src)
echo "--- Running Python Smoke Test ---"
export PYTHONPATH=$PYTHONPATH:$(pwd)/clients/py/src
python3 -c "
import sys
import time
from entroq.json import EntroQJSON
from entroq.types import TaskData

client = EntroQJSON('http://localhost:9100')
inserted, _ = client.modify(inserts=[TaskData(queue='/smoke/py', value=b'py-work')])
task = client.claim(['/smoke/py'])
if task.value != b'py-work':
    print(f'Mismatched value: {task.value}')
    sys.exit(1)
client.modify(deletes=[task.id_version])
print('Python claim/modify success!')
"

# 5. JS/TS Smoke Test (Node)
echo "--- Running JS Smoke Test ---"
(cd clients/js && npm install --no-package-lock && npm run build > /dev/null 2>&1)
export NODE_PATH=$(pwd)/clients/js/node_modules
node -e "
const { EntroQClient } = require('./clients/js/dist/client');
(async () => {
    const client = new EntroQClient({ baseUrl: 'http://localhost:9100' });
    try {
        await client.modify({ 
            inserts: [{ queue: '/smoke/js', value: Buffer.from('js-work').toString('base64') }] 
        });
        const resp = await client.claim(['/smoke/js']);
        if (Buffer.from(resp.task.value, 'base64').toString() !== 'js-work') {
            throw new Error('Mismatched value: ' + resp.task.value);
        }
        await client.modify({ 
            deletes: [{ id: resp.task.id, version: resp.task.version, queue: '/smoke/js' }] 
        });
        console.log('JS claim/modify success!');
    } catch (err) {
        console.error(err);
        process.exit(1);
    }
})();
"

echo "--- ALL SMOKE TESTS PASSED ---"
