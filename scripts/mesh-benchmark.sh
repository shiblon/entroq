#!/bin/sh
# Run the local-cluster EntroQ mesh benchmark on a disposable k3d cluster.
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
cd "$repo_root"
entroq_source_root=${ENTROQ_SOURCE_ROOT:-$repo_root}
case "$entroq_source_root" in
    /*) ;;
    *) entroq_source_root=$(CDPATH='' cd -- "$entroq_source_root" && pwd) ;;
esac
run_id=${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
cluster_name=${CLUSTER_NAME:-entroq-mesh-${run_id}}
k3s_image=${K3S_IMAGE:-rancher/k3s:v1.35.5-k3s1}
image_tag=${IMAGE_TAG:-meshbench-${run_id}}
build_images=${BUILD_IMAGES:-1}
samples=${SAMPLES:-3}
duration=${DURATION:-15s}
warmup=${WARMUP:-3s}
concurrency=${CONCURRENCY:-8}
target_rps=${TARGET_RPS:-0}
payload_bytes=${PAYLOAD_BYTES:-1024}
request_timeout=${REQUEST_TIMEOUT:-10s}
metric_interval=${METRIC_INTERVAL:-1s}
authz_strategy=${AUTHZ_STRATEGY:-opahttp}
opa_policy_mode=${OPA_POLICY_MODE:-full}
backend=${BACKEND:-memory}
redis_image=${REDIS_IMAGE:-redis:7-alpine}
postgres_image=${POSTGRES_IMAGE:-postgres:17}
entroq_cpu_limit=${ENTROQ_CPU_LIMIT:-500m}
opa_cpu_limit=${OPA_CPU_LIMIT:-250m}
keep_cluster=${KEEP_CLUSTER:-0}
result_dir=${RESULT_DIR:-${repo_root}/benchmarks/mesh/results/${run_id}}
kubeconfig_file=${result_dir}/kubeconfig
cluster_created=0

log() {
    printf '%s\n' "$*" >&2
}

if [ "${#cluster_name}" -gt 32 ]; then
    log "cluster name must be at most 32 characters: ${cluster_name}"
    exit 1
fi

need() {
    if ! command -v "$1" >/dev/null 2>&1; then
        log "missing required command: $1"
        exit 1
    fi
}

cleanup() {
    if [ "$cluster_created" = 1 ] && [ "$keep_cluster" != 1 ]; then
        log "deleting disposable cluster ${cluster_name}"
        k3d cluster delete "$cluster_name" >/dev/null
    elif [ "$cluster_created" = 1 ]; then
        log "keeping cluster ${cluster_name}; use: k3d cluster delete ${cluster_name}"
    fi
    rm -f "$kubeconfig_file"
}
trap cleanup EXIT HUP INT TERM

for command_name in docker k3d kubectl helm go make sed; do
    need "$command_name"
done

case "$samples:$concurrency:$payload_bytes" in
    *[!0-9:]* | 0:* | *:0:* | *:0)
        log "SAMPLES, CONCURRENCY, and PAYLOAD_BYTES must be positive integers"
        exit 1
        ;;
esac
case "$build_images" in
    0 | 1) ;;
    *)
        log "BUILD_IMAGES must be 0 or 1"
        exit 1
        ;;
esac
case "$authz_strategy" in
    opahttp)
        operator_enabled=true
        oidc_discovery=true
        opa_metric='        - --metric=opa=http://entroq.entroq-system.svc.cluster.local:8181/metrics'
        case "$opa_policy_mode" in
            full)
                authz_profile=full
                opa_path=/v1/data/entroq/authz
                ;;
            allow-all)
                authz_profile=allow-all
                opa_path=/v1/data/meshbench/authz
                ;;
            *)
                log "OPA_POLICY_MODE must be full or allow-all"
                exit 1
                ;;
        esac
        ;;
    none)
        if [ "$opa_policy_mode" != full ]; then
            log "OPA_POLICY_MODE applies only when AUTHZ_STRATEGY=opahttp"
            exit 1
        fi
        operator_enabled=false
        oidc_discovery=false
        opa_metric=
        authz_profile=none
        opa_path=/v1/data/entroq/authz
        ;;
    *)
        log "AUTHZ_STRATEGY must be opahttp or none"
        exit 1
        ;;
esac

case "$backend" in
    memory)
        server_image=entroq-mem
        server_image_key=mem
        server_dockerfile=cmd/eqmem/Dockerfile
        backend_durability='in-memory; data lost on restart'
        ;;
    redis)
        server_image=entroq-redis
        server_image_key=redis
        server_dockerfile=cmd/eqredis/Dockerfile
        backend_durability='Redis 7 with RDB snapshots and AOF disabled'
        ;;
    postgres)
        server_image=entroq-pg
        server_image_key=postgres
        server_dockerfile=cmd/eqpg/Dockerfile
        backend_durability='PostgreSQL 17 container defaults on ephemeral storage'
        ;;
    *)
        log "BACKEND must be memory, redis, or postgres"
        exit 1
        ;;
esac

mkdir -p "$result_dir"

if [ ! -f "$entroq_source_root/go.mod" ] || [ ! -d "$entroq_source_root/charts/entroq" ]; then
    log "ENTROQ_SOURCE_ROOT is not an EntroQ source tree: ${entroq_source_root}"
    exit 1
fi

log "syncing generated Helm Rego and CRD artifacts"
make -C "$entroq_source_root" helm-sync

if [ "$build_images" = 1 ]; then
    log "building local images with tag ${image_tag}"
    docker build --build-arg VERSION=meshbench -f "$entroq_source_root/$server_dockerfile" -t "${server_image}:${image_tag}" "$entroq_source_root"
    if [ "$authz_strategy" = opahttp ]; then
        docker build --build-arg VERSION=meshbench -f "$entroq_source_root/cmd/eqk8s/Dockerfile" -t "entroq-operator:${image_tag}" "$entroq_source_root"
    fi
    docker build --build-arg VERSION=meshbench -f cmd/eqlink/Dockerfile -t "entroq-link:${image_tag}" "$repo_root"
    docker build -f benchmarks/mesh/Dockerfile -t "entroq-mesh-workload:${image_tag}" "$repo_root"
else
    log "reusing local images with tag ${image_tag}"
fi

log "creating ${cluster_name} with four agents from ${k3s_image}"
k3d cluster create "$cluster_name" \
    --image "$k3s_image" \
    --servers 1 \
    --agents 4 \
    --k3s-arg '--disable=traefik@server:*' \
    --k3s-arg '--kube-apiserver-arg=anonymous-auth=true@server:*' \
    --kubeconfig-update-default=false \
    --kubeconfig-switch-context=false \
    --timeout 3m \
    --wait
cluster_created=1
k3d kubeconfig get "$cluster_name" >"$kubeconfig_file"
export KUBECONFIG="$kubeconfig_file"

log "importing local images"
if [ "$authz_strategy" = opahttp ]; then
    k3d image import --cluster "$cluster_name" \
        "${server_image}:${image_tag}" \
        "entroq-operator:${image_tag}" \
        "entroq-link:${image_tag}" \
        "entroq-mesh-workload:${image_tag}"
else
    k3d image import --cluster "$cluster_name" \
        "${server_image}:${image_tag}" \
        "entroq-link:${image_tag}" \
        "entroq-mesh-workload:${image_tag}"
fi

{
    printf 'run_id=%s\n' "$run_id"
    printf 'cluster_name=%s\n' "$cluster_name"
    printf 'entroq_source_root=%s\n' "$entroq_source_root"
    printf 'k3s_image=%s\n' "$k3s_image"
    printf 'image_tag=%s\n' "$image_tag"
    printf 'build_images=%s\n' "$build_images"
    printf 'samples=%s\n' "$samples"
    printf 'duration=%s\n' "$duration"
    printf 'warmup=%s\n' "$warmup"
    printf 'concurrency=%s\n' "$concurrency"
    printf 'target_rps=%s\n' "$target_rps"
    printf 'payload_bytes=%s\n' "$payload_bytes"
    printf 'request_timeout=%s\n' "$request_timeout"
    printf 'metric_interval=%s\n' "$metric_interval"
    printf 'authz_strategy=%s\n' "$authz_strategy"
    printf 'authz_profile=%s\n' "$authz_profile"
    printf 'backend=%s\n' "$backend"
    printf 'backend_durability=%s\n' "$backend_durability"
    printf 'entroq_cpu_limit=%s\n' "$entroq_cpu_limit"
    printf 'opa_cpu_limit=%s\n' "$opa_cpu_limit"
    k3d version
    kubectl version --client=true
    helm version --short
} >"$result_dir/environment.txt"
kubectl get nodes -o wide >"$result_dir/nodes.txt"

case "$backend" in
    redis)
        sed "s|@REDIS_IMAGE@|${redis_image}|g" "$repo_root/benchmarks/mesh/k8s/redis.yaml" | kubectl apply -f -
        kubectl rollout status deployment/mesh-redis --timeout=3m
        ;;
    postgres)
        sed "s|@POSTGRES_IMAGE@|${postgres_image}|g" "$repo_root/benchmarks/mesh/k8s/postgres.yaml" | kubectl apply -f -
        kubectl rollout status deployment/mesh-postgres --timeout=3m
        ;;
esac

log "installing EntroQ ${backend} backend with authorization strategy ${authz_strategy}"
helm upgrade --install entroq "$entroq_source_root/charts/entroq" \
    --namespace default \
    --set "operator.image.repository=entroq-operator" \
    --set "operator.image.tag=${image_tag}" \
    --set "operator.image.pullPolicy=Never" \
    --set "operator.enabled=${operator_enabled}" \
    --set "entroq.backend.type=${backend}" \
    --set "entroq.authorization.strategy=${authz_strategy}" \
    --set-string "entroq.authorization.opaPath=${opa_path}" \
    --set "entroq.images.${server_image_key}.repository=${server_image}" \
    --set "entroq.images.${server_image_key}.tag=${image_tag}" \
    --set "entroq.images.pullPolicy=Never" \
    --set "entroq.redis.addr=mesh-redis.default.svc.cluster.local:6379" \
    --set "entroq.postgres.addr=mesh-postgres.default.svc.cluster.local:5432" \
    --set "entroq.postgres.password=meshbench" \
    --set "entroq.resources.entroq.limits.cpu=${entroq_cpu_limit}" \
    --set "entroq.resources.opa.limits.cpu=${opa_cpu_limit}" \
    --set "oidcDiscovery.grantAnonymous=${oidc_discovery}" \
    --wait \
    --timeout 3m

kubectl create namespace mesh-bench --save-config
if [ "$authz_profile" = allow-all ]; then
    sed "s/@IMAGE_TAG@/${image_tag}/g" "$repo_root/benchmarks/mesh/k8s/opa-policy-job.yaml" | kubectl apply -f -
    kubectl wait --for=condition=complete job/meshbench-opa-policy -n mesh-bench --timeout=1m
    kubectl logs job/meshbench-opa-policy -n mesh-bench
    kubectl delete job meshbench-opa-policy -n mesh-bench --wait=true >/dev/null
fi
sed "s/@IMAGE_TAG@/${image_tag}/g" "$repo_root/benchmarks/mesh/k8s/workloads.yaml" | kubectl apply -f -
if [ "$authz_strategy" = opahttp ]; then
    sed \
        -e "s/@IMAGE_TAG@/${image_tag}/g" \
        -e "s|@OPA_PATH@|${opa_path}|g" \
        "$repo_root/benchmarks/mesh/k8s/direct-auth.yaml" | kubectl apply -f -
fi
kubectl rollout status deployment/gateway -n mesh-bench --timeout=3m
kubectl rollout status deployment/relay -n mesh-bench --timeout=3m
kubectl rollout status deployment/leaf -n mesh-bench --timeout=3m
if [ "$authz_strategy" = opahttp ]; then
    kubectl rollout status deployment/direct-auth -n mesh-bench --timeout=3m
fi
# Deployment readiness can precede kube-proxy observing the new Service
# endpoints. Keep startup convergence outside the strict smoke warm-up.
sleep 2

wait_job() {
    job_name=$1
    output_file=$2
    resource_file=${output_file%.json}-resources.txt
    deadline=$(( $(date +%s) + 180 ))
    : >"$resource_file"
    while :; do
        succeeded=$(kubectl get job "$job_name" -n mesh-bench -o 'jsonpath={.status.succeeded}' 2>/dev/null || true)
        failed=$(kubectl get job "$job_name" -n mesh-bench -o 'jsonpath={.status.failed}' 2>/dev/null || true)
        if [ "$succeeded" = 1 ]; then
            kubectl logs "job/${job_name}" -n mesh-bench >"$output_file"
            return 0
        fi
        if [ -n "$failed" ] && [ "$failed" -ge 1 ]; then
            kubectl logs "job/${job_name}" -n mesh-bench >"$output_file" || true
            log "job ${job_name} failed; logs saved to ${output_file}"
            return 1
        fi
        if [ "$(date +%s)" -ge "$deadline" ]; then
            kubectl describe job "$job_name" -n mesh-bench >&2 || true
            log "job ${job_name} timed out"
            return 1
        fi
        {
            date -u '+sampled_at=%Y-%m-%dT%H:%M:%SZ'
            kubectl top pods -A --containers --no-headers
        } >>"$resource_file" 2>/dev/null || true
        sleep 1
    done
}

run_sample() {
    mode=$1
    sample=$2
    measured=$3
    sample_warmup=$4
    output_file=$5
    sample_target_rps=${6:-$target_rps}
    job_name="meshbench-${mode}-${sample}"
    case "$mode" in
        direct-raw)
            url=http://leaf-direct.mesh-bench.svc.cluster.local:8080/work
            host=
            expected_status=200
            ;;
        direct-auth)
            url=http://direct-auth.mesh-bench.svc.cluster.local:8080/work
            host=leaf.localhost
            expected_status=200
            ;;
        direct-denied)
            url=http://direct-auth.mesh-bench.svc.cluster.local:8080/work
            host=denied.localhost
            expected_status=403
            ;;
        mesh)
            url=http://gateway-mesh.mesh-bench.svc.cluster.local:8080/work
            host=leaf.localhost
            expected_status=200
            ;;
        mesh2)
            url=http://gateway-mesh.mesh-bench.svc.cluster.local:8080/work
            host=relay.localhost
            expected_status=200
            ;;
        *)
            log "unknown benchmark mode: ${mode}"
            return 1
            ;;
    esac

    sed \
        -e "s/@JOB_NAME@/${job_name}/g" \
        -e "s/@IMAGE_TAG@/${image_tag}/g" \
        -e "s/@MODE@/${mode}/g" \
        -e "s/@AUTHZ_STRATEGY@/${authz_strategy}/g" \
        -e "s/@AUTHZ_PROFILE@/${authz_profile}/g" \
        -e "s/@SAMPLE@/${sample}/g" \
        -e "s|@URL@|${url}|g" \
        -e "s/@HOST@/${host}/g" \
        -e "s/@CONCURRENCY@/${concurrency}/g" \
        -e "s/@TARGET_RPS@/${sample_target_rps}/g" \
        -e "s/@DURATION@/${measured}/g" \
        -e "s/@WARMUP@/${sample_warmup}/g" \
        -e "s/@PAYLOAD_BYTES@/${payload_bytes}/g" \
        -e "s/@EXPECTED_STATUS@/${expected_status}/g" \
        -e "s/@REQUEST_TIMEOUT@/${request_timeout}/g" \
        -e "s/@METRIC_INTERVAL@/${metric_interval}/g" \
        -e "s|@OPA_METRIC@|${opa_metric}|g" \
        "$repo_root/benchmarks/mesh/k8s/job.yaml" | kubectl apply -f -
    if ! wait_job "$job_name" "$output_file"; then
        return 1
    fi
    kubectl delete job "$job_name" -n mesh-bench --wait=true >/dev/null
}

# Warm each smoke path before strict metric sampling; rollout readiness can
# precede Service endpoint propagation by a small interval.
log "proving the one-hop mesh path with authorization strategy ${authz_strategy}"
run_sample mesh 1 1s "$warmup" "$result_dir/smoke-mesh.json" 0
log "waiting for the two-hop relay path"
run_sample mesh2 1 1s "$warmup" "$result_dir/smoke-mesh2.json" 0
if [ "$authz_strategy" = opahttp ]; then
    log "proving the authorized direct path"
    run_sample direct-auth 1 1s "$warmup" "$result_dir/smoke-direct-auth.json" 0
fi
if [ "$authz_profile" = full ]; then
    log "proving per-service denial on the authorized direct path"
    run_sample direct-denied 1 1s "$warmup" "$result_dir/smoke-direct-denied.json" 0
fi

sample=1
while [ "$sample" -le "$samples" ]; do
    if [ "$authz_strategy" = opahttp ]; then
        case $((sample % 4)) in
            1) order='direct-raw direct-auth mesh mesh2' ;;
            2) order='direct-auth mesh mesh2 direct-raw' ;;
            3) order='mesh mesh2 direct-raw direct-auth' ;;
            0) order='mesh2 direct-raw direct-auth mesh' ;;
        esac
    else
        case $((sample % 3)) in
            1) order='direct-raw mesh mesh2' ;;
            2) order='mesh mesh2 direct-raw' ;;
            0) order='mesh2 direct-raw mesh' ;;
        esac
    fi
    for mode in $order; do
        output_file=$(printf '%s/sample-%02d-%s.json' "$result_dir" "$sample" "$mode")
        log "running sample ${sample}/${samples}: ${mode}"
        run_sample "$mode" "$sample" "$duration" "$warmup" "$output_file"
    done
    sample=$((sample + 1))
done

kubectl get pods -A -o wide >"$result_dir/pods.txt"
kubectl get events -A --sort-by=.lastTimestamp >"$result_dir/events.txt"
GOCACHE=/tmp/entroq-mesh-report-cache \
GOMODCACHE=/tmp/entroq-mesh-report-mod-cache \
go run ./benchmarks/mesh/workload report \
    --input-dir "$result_dir" \
    --backend "$backend" >"$result_dir/summary.md"

log "benchmark complete: ${result_dir}/summary.md"
