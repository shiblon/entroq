#!/bin/bash

cd "$(dirname "$0")" >/dev/null 2>&1

docker run -it --rm -v $PWD:/test/opa openpolicyagent/opa:0.26.0 test /test/opa -v
