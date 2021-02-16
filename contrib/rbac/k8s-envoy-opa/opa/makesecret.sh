#!/bin/bash

cd "$(dirname "$0")" >/dev/null 2>&1

kubectl create secret generic opa-policy --from-file policy.rego
