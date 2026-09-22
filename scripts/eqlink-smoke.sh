#!/bin/sh
# Exercise EQLink streaming and failure recovery in a disposable, low-memory k3d cluster.
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
cd "$repo_root"

run_id=${RUN_ID:-$(date -u +%Y%m%d%H%M%S)}
cluster_name=${CLUSTER_NAME:-eqstream-${run_id}}
k3s_image=${K3S_IMAGE:-rancher/k3s:v1.35.5-k3s1}
image_tag=${IMAGE_TAG:-eqlink-smoke-${run_id}}
image=entroq-eqlink-smoke:${image_tag}
keep_cluster=${KEEP_CLUSTER:-0}
result_dir=${RESULT_DIR:-${repo_root}/benchmarks/mesh/results/eqlink-smoke-${run_id}}
kubeconfig_file=${result_dir}/kubeconfig
build_dir=${result_dir}/build
gocache_dir=${result_dir}/gocache
manifest_file=${result_dir}/manifest.yaml
cluster_created=0
image_created=0
load_pid=

log() {
    printf '%s\n' "$*" >&2
}

fail() {
    log "FAIL: $*"
    exit 1
}

need() {
    if ! command -v "$1" >/dev/null 2>&1; then
        fail "missing required command: $1"
    fi
}

cleanup() {
    if [ -n "$load_pid" ]; then
        kill "$load_pid" >/dev/null 2>&1 || true
        wait "$load_pid" >/dev/null 2>&1 || true
    fi
    rm -rf "$build_dir" "$gocache_dir"
    if [ "$cluster_created" = 1 ] && [ "$keep_cluster" != 1 ]; then
        log "deleting disposable cluster ${cluster_name}"
        if k3d cluster delete "$cluster_name" >/dev/null; then
            cluster_created=0
        else
            log "WARNING: cluster deletion failed; preserving image and kubeconfig"
            log "cleanup command: k3d cluster delete ${cluster_name}"
        fi
    elif [ "$cluster_created" = 1 ]; then
        log "keeping cluster ${cluster_name}; use: k3d cluster delete ${cluster_name}"
    fi
    if [ "$image_created" = 1 ] && [ "$keep_cluster" != 1 ] && [ "$cluster_created" = 0 ]; then
        docker image rm "$image" >/dev/null 2>&1 || true
        image_created=0
    fi
    if [ "$keep_cluster" != 1 ] && [ "$cluster_created" = 0 ]; then
        rm -f "$kubeconfig_file"
    fi
}

case "$result_dir" in
    "$repo_root"/benchmarks/mesh/results/*) ;;
    *) fail "RESULT_DIR must be beneath ${repo_root}/benchmarks/mesh/results" ;;
esac
case "$cluster_name" in
    *[!a-z0-9-]* | '') fail "CLUSTER_NAME must contain only lowercase letters, digits, and hyphens" ;;
esac
if [ "${#cluster_name}" -gt 32 ]; then
    fail "CLUSTER_NAME must be at most 32 characters: ${cluster_name}"
fi
case "$keep_cluster" in
    0 | 1) ;;
    *) fail "KEEP_CLUSTER must be 0 or 1" ;;
esac
if [ -e "$result_dir" ]; then
    fail "result directory already exists: ${result_dir}"
fi
trap cleanup EXIT HUP INT TERM

for command_name in docker k3d kubectl go sed; do
    need "$command_name"
done
if k3d cluster list --no-headers | awk '{print $1}' | grep -Fx "$cluster_name" >/dev/null 2>&1; then
    fail "cluster already exists: ${cluster_name}"
fi

mkdir -p "$build_dir/bin" "$gocache_dir"
log "building static EntroQ, EQLink, and duplex probe binaries"
CGO_ENABLED=0 GOCACHE="$gocache_dir" go build -trimpath -ldflags=-s -o "$build_dir/bin/eqmem" ./cmd/eqmem
CGO_ENABLED=0 GOCACHE="$gocache_dir" go build -trimpath -ldflags=-s -o "$build_dir/bin/eqlink" ./cmd/eqlink
CGO_ENABLED=0 GOCACHE="$gocache_dir" go build -trimpath -ldflags=-s -o "$build_dir/bin/probe" ./benchmarks/mesh/eqlink-smoke/workload.go
cp benchmarks/mesh/eqlink-smoke/Dockerfile "$build_dir/Dockerfile"

log "building scratch image ${image}"
docker build -t "$image" "$build_dir"
image_created=1

log "creating one-node cluster ${cluster_name} with a 2 GiB memory cap"
cluster_created=1
k3d cluster create "$cluster_name" \
    --image "$k3s_image" \
    --servers 1 \
    --agents 0 \
    --servers-memory 2g \
    --no-lb \
    --k3s-arg '--disable=traefik@server:*' \
    --k3s-arg '--disable=servicelb@server:*' \
    --k3s-arg '--disable=metrics-server@server:*' \
    --kubeconfig-update-default=false \
    --kubeconfig-switch-context=false \
    --timeout 2m \
    --wait
mkdir -p "$result_dir"
k3d kubeconfig get "$cluster_name" >"$kubeconfig_file"
export KUBECONFIG="$kubeconfig_file"

log "importing the local image and starting all three pods together"
k3d image import --cluster "$cluster_name" "$image"
sed "s|@IMAGE@|${image}|g" benchmarks/mesh/eqlink-smoke/manifest.yaml >"$manifest_file"
kubectl apply -f "$manifest_file"
for deployment in entroq sender receiver; do
    kubectl rollout status "deployment/${deployment}" -n eqlink-smoke --timeout=90s
done

# Give a process that lost a startup race enough time to exit before checking.
# EQLink's bounded blocking dial should make every restart count stay at zero.
sleep 2
pod_count=$(kubectl get pods -n eqlink-smoke --no-headers | wc -l | tr -d ' ')
[ "$pod_count" = 3 ] || fail "expected exactly three pods, found ${pod_count}"
for restarts in $(kubectl get pods -n eqlink-smoke -o 'jsonpath={range .items[*].status.containerStatuses[*]}{.restartCount}{"\n"}{end}'); do
    [ "$restarts" = 0 ] || fail "a container restarted during initial startup"
done
log "PASS startup: three ready pods, zero container restarts"

probe_sender() {
    kubectl exec -n eqlink-smoke deployment/sender -c probe -- \
        /bin/probe load --url=http://localhost:8080/duplex --host=receiver.test "$@"
}

wait_deployment() {
    kubectl rollout status "deployment/$1" -n eqlink-smoke --timeout=60s
    kubectl wait --for=condition=Ready pod -n eqlink-smoke -l "app=$1" --timeout=60s
}

expect_load_failure() {
    phase=$1
    output=$2
    if wait "$load_pid"; then
        load_pid=
        fail "${phase}: active streams unexpectedly survived an abrupt transport loss"
    fi
    load_pid=
    log "PASS ${phase}: active streams failed without false success"
    sed 's/^/  /' "$output" >&2
}

log "baseline: 24 concurrent sessions, eight lock-step frames each"
probe_sender --sessions=24 --frames=8 --payload-bytes=8192 --delay=5ms --timeout=30s

log "abrupt receiver crash: active streams must fail, fresh streams must recover"
receiver_failure=${result_dir}/abrupt-receiver.log
probe_sender --sessions=4 --frames=40 --payload-bytes=8192 --delay=250ms --timeout=12s >"$receiver_failure" 2>&1 &
load_pid=$!
sleep 1
receiver_pod=$(kubectl get pods -n eqlink-smoke -l app=receiver -o 'jsonpath={.items[0].metadata.name}')
kubectl delete pod -n eqlink-smoke "$receiver_pod" --grace-period=0 --force
expect_load_failure "abrupt receiver crash" "$receiver_failure"
wait_deployment receiver
probe_sender --sessions=8 --frames=8 --payload-bytes=8192 --delay=5ms --timeout=15s

log "EntroQ outage: active streams must fail, restored service must recover"
entroq_failure=${result_dir}/entroq-outage.log
probe_sender --sessions=4 --frames=40 --payload-bytes=8192 --delay=250ms --timeout=12s >"$entroq_failure" 2>&1 &
load_pid=$!
sleep 1
kubectl scale deployment/entroq -n eqlink-smoke --replicas=0
expect_load_failure "EntroQ outage" "$entroq_failure"
kubectl scale deployment/entroq -n eqlink-smoke --replicas=1
wait_deployment entroq
wait_deployment receiver
probe_sender --sessions=8 --frames=8 --payload-bytes=8192 --delay=5ms --timeout=15s

log "abrupt sender crash: active streams must fail, fresh streams must recover"
sender_failure=${result_dir}/abrupt-sender.log
sender_pod=$(kubectl get pods -n eqlink-smoke -l app=sender -o 'jsonpath={.items[0].metadata.name}')
sender_ip=$(kubectl get pod -n eqlink-smoke "$sender_pod" -o 'jsonpath={.status.podIP}')
kubectl exec -n eqlink-smoke deployment/entroq -c entroq -- \
    /bin/probe load --url="http://${sender_ip}:8080/duplex" --host=receiver.test \
    --sessions=4 --frames=40 --payload-bytes=8192 --delay=250ms --timeout=12s >"$sender_failure" 2>&1 &
load_pid=$!
sleep 1
kubectl delete pod -n eqlink-smoke "$sender_pod" --grace-period=0 --force
expect_load_failure "abrupt sender crash" "$sender_failure"
wait_deployment sender
probe_sender --sessions=8 --frames=8 --payload-bytes=8192 --delay=5ms --timeout=15s

kubectl get pods -n eqlink-smoke -o wide >"$result_dir/pods.txt"
kubectl get events -n eqlink-smoke --sort-by=.lastTimestamp >"$result_dir/events.txt"
kubectl logs -n eqlink-smoke deployment/sender -c eqlink >"$result_dir/sender.log"
kubectl logs -n eqlink-smoke deployment/receiver -c eqlink >"$result_dir/receiver.log"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}} {{.CPUPerc}}' \
    "k3d-${cluster_name}-server-0" >"$result_dir/resources.txt"

printf '%s\n' \
    'PASS: EQLink low-memory mesh smoke test' \
    '  startup: three pods, zero restarts' \
    '  baseline: concurrent bidirectional streams' \
    '  abrupt receiver crash: active streams failed; fresh streams recovered' \
    '  EntroQ outage: active streams failed; fresh streams recovered' \
    '  abrupt sender crash: active streams failed; fresh streams recovered' \
    >"$result_dir/summary.txt"
cat "$result_dir/summary.txt"
log "artifacts: ${result_dir}"
