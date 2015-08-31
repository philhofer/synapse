#! /bin/bash
set -e
ssts &
SSTS_PID=$!
sleep 0.1
set +e
sstc
RET=$?
eval kill -s SIGINT $SSTS_PID || echo "Couldn't kill server."
exit $RET
