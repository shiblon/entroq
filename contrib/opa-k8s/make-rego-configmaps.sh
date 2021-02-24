#!/bin/bash

cd "$(dirname "$0")"

tmp="tmp"
dest="opa"

mkdir -p $tmp/{core,example}

cat <<EOF > $tmp/core/kustomization.yaml
configMapGenerator:
  - name: eqopa-core
    files:
      - core-entroq-authz.rego
      - core-entroq-queues.rego
EOF

cat <<EOF > $tmp/example/kustomization.yaml
configMapGenerator:
  - name: eqopa-example
    files:
      - example-entroq-permissions.rego
      - example-entroq-user.rego
EOF

data_dir="$(cd "../../pkg/authz/opadata" >/dev/null 2>&1; pwd -P)"
cp "$data_dir/core/"*.rego $tmp/core

# Also copy examples
cp "$data_dir/example/"*.rego $tmp/example

kubectl kustomize $tmp/core \
  | sed -e 's/name: eqopa-.*/name: eqopa-core/' \
  > $dest/cm-eqopa-core.yaml

kubectl kustomize $tmp/example \
  | sed -e 's/name: eqopa-.*/name: eqopa-example/' \
  > $dest/cm-eqopa-example.yaml
