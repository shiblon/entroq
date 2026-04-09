#!/bin/bash
./eqmemsvc --http_port 9108 --port 37713 > server.log 2>&1 &
SVCPID=$!
sleep 3
if ps -p $SVCPID > /dev/null; then
  echo "Server is running"
  curl -v "http://localhost:9108/api/v0/time"
  kill $SVCPID
else
  echo "Server failed to start"
  cat server.log
fi
