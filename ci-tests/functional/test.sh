#!/usr/bin/env bash
# functional hook — mo-cicd runs it as pre-merge:functional on every merge request,
# with a Docker daemon (dind) and docker/kind/kubectl/helm on PATH. Locally: make test-e2e.
#
# Replaces the kubebuilder e2e scaffold (test/e2e), which never ran in CI. It deploys
# config/default (the kustomize manifests) with this MR's image on a throwaway Kind
# cluster and checks:
#   1. the manager rolls out and takes the leader-election lease;
#   2. the metrics endpoint serves, with authentication (what the scaffold checked);
#   3. a LoadBalancer Service gets the controller's finalizer (the reconcile loop runs);
#   4. a Service with another loadBalancerClass is left alone;
#   5. the manager did not restart.
# There are no Crusoe credentials and CRUSOE_API_ENDPOINT points at a closed local
# port: reconcile fails after step 3 by design, and no call leaves the cluster.
#
# Provided by the job (mo-cicd scripts/run-functional.sh):
#   MO_IMAGES         this pipeline's images, pushed as <name>:<sha> by pre-merge:images
#   MO_KIND_CLUSTER   a unique cluster name; every Kind cluster is deleted when the hook ends
#   MO_REPORTS_DIR    JUnit files dropped here (junit-functional-*.xml) show on the MR
set -euo pipefail

NS=crusoe-load-balancer-controller-system
DEPLOY=crusoe-lb-controller-controller-manager
SA=crusoe-lb-controller-controller-manager
METRICS_SVC=crusoe-lb-controller-controller-manager-metrics-service
METRICS_READER=crusoe-lb-controller-metrics-reader
FINALIZER=crusoe.ai/crusoe-load-balancer-controller.finalizer
LEASE=a451eb4c.crusoecloud.com           # LeaderElectionID in cmd/main.go
TEST_NS=lbc-functional
CLUSTER="${MO_KIND_CLUSTER:?MO_KIND_CLUSTER is not set}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$ROOT/.mo-cicd-out/functional"   # inside the repo: kustomize loads ../../config/default
reports="${MO_REPORTS_DIR:-$WORK}"
mkdir -p "$WORK" "$reports"
# Own kubeconfig: kind and kubectl must not change or follow the caller's current context.
export KUBECONFIG="$WORK/kubeconfig"
rm -f "$KUBECONFIG"
log() { printf '== %s\n' "$*"; }

# <name>:<sha> for the controller image, from MO_IMAGES.
image_ref() {
  local ref
  for ref in ${MO_IMAGES//,/ }; do
    case "${ref%:*}" in
      */crusoe-load-balancer-controller | lbc-e2e) echo "$ref"; return 0 ;;
    esac
  done
  echo "controller image not in MO_IMAGES (${MO_IMAGES:-})" >&2; return 1
}
image="$(image_ref)"

# JUnit: one testcase per phase, so a failure names what broke on the MR.
cases=(); failed=0; pf_pid=""
phase() {   # name cmd...
  local name="$1"; shift
  local start rc=0; start=$(date +%s)
  # Bash ignores set -e in a command whose status is tested (`x || y`, `if x`), even
  # inside a subshell. So phase stops the run itself and no caller tests its status.
  log "$name"; set +e; ( set -e; "$@" ); rc=$?; set -e
  local secs=$(( $(date +%s) - start ))
  if (( rc == 0 )); then cases+=("<testcase name=\"$name\" time=\"$secs\"/>"); return 0; fi
  cases+=("<testcase name=\"$name\" time=\"$secs\"><failure message=\"exit $rc\"/></testcase>"); failed=$((failed + 1))
  # Before deploy there is nothing of ours to dump.
  [[ "$name" == create-cluster || "$name" == load-image ]] || dump_on_failure
  exit "$rc"
}
finish() {
  { echo "<testsuite name=\"functional\" tests=\"${#cases[@]}\" failures=\"$failed\">"
    printf '%s\n' "${cases[@]}"; echo "</testsuite>"; } > "$reports/junit-functional.xml"
  [[ -n "$pf_pid" ]] && kill "$pf_pid" 2>/dev/null || true
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
# The EXIT trap deletes $CLUSTER, so refuse a cluster that this run did not create.
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "Kind cluster '$CLUSTER' exists. Delete it or set another MO_KIND_CLUSTER." >&2; exit 1
fi
trap finish EXIT

create_cluster() {
  kind create cluster --name "$CLUSTER" --wait 2m
}
load_image() {
  docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
  kind load docker-image --name "$CLUSTER" "$image"
}
deploy() {
  cat > "$WORK/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../config/default
images:
  - name: ghcr.io/crusoecloud/crusoe-load-balancer-controller
    newName: ${image%:*}
    newTag: ${image##*:}
patches:
  - target: { kind: Deployment }
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/env
        value:
          - { name: CRUSOE_API_ENDPOINT, value: "http://127.0.0.1:9/v1alpha5" }
EOF
  kubectl apply -k "$WORK"
  kubectl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=3m
}
leader_lease() {
  # The Lease object, not the log line: its holder is the manager pod once elected.
  local holder=""
  for _ in $(seq 1 45); do
    holder="$(kubectl -n "$NS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)"
    [[ "$holder" == "$DEPLOY"-* ]] && return 0
    sleep 2
  done
  echo "lease $LEASE not held by the manager after 90s (holder: '${holder}')" >&2; return 1
}
metrics() {
  kubectl create clusterrolebinding lbc-functional-metrics \
    --clusterrole="$METRICS_READER" --serviceaccount="$NS:$SA"
  local token
  token="$(kubectl -n "$NS" create token "$SA")"
  kubectl -n "$NS" port-forward "service/$METRICS_SVC" 18443:8443 >"$WORK/port-forward.log" 2>&1 &
  pf_pid=$!
  for _ in $(seq 1 30); do
    if curl -skf -H "Authorization: Bearer $token" https://127.0.0.1:18443/metrics >"$WORK/metrics.txt"; then
      grep -q '^controller_runtime_reconcile_total' "$WORK/metrics.txt" && return 0
    fi
    sleep 2
  done
  echo "metrics endpoint did not serve controller_runtime_reconcile_total" >&2; return 1
}
services() {
  kubectl create namespace "$TEST_NS"
  kubectl -n "$TEST_NS" apply -f - <<'EOF'
apiVersion: v1
kind: Service
metadata: { name: other-class }
spec:
  type: LoadBalancer
  loadBalancerClass: example.com/other
  selector: { app: none }
  ports: [{ port: 80, protocol: TCP }]
---
apiVersion: v1
kind: Service
metadata: { name: ours }
spec:
  type: LoadBalancer
  selector: { app: none }
  ports: [{ port: 80, protocol: TCP }]
EOF
}
finalizer_of() { kubectl -n "$TEST_NS" get service "$1" -o jsonpath='{.metadata.finalizers}'; }
reconciles_ours() {
  for _ in $(seq 1 30); do
    [[ "$(finalizer_of ours)" == *"$FINALIZER"* ]] && return 0
    sleep 2
  done
  echo "service 'ours' has no $FINALIZER after 60s" >&2; return 1
}
skips_other_class() {
  # Checked after 'ours' was reconciled, so the controller had its chance.
  local got; got="$(finalizer_of other-class)"
  [[ "$got" != *"$FINALIZER"* ]] || { echo "service 'other-class' got $FINALIZER: $got" >&2; return 1; }
}
no_restarts() {
  local restarts
  restarts="$(kubectl -n "$NS" get pods -l control-plane=controller-manager \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort -n | tail -1)"
  [[ "$restarts" == "0" ]] || { echo "manager restarted $restarts time(s)" >&2; return 1; }
}
dump_on_failure() {
  kubectl -n "$NS" get pods -o wide || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -40 || true
  kubectl -n "$NS" logs "deployment/$DEPLOY" --tail=100 || true
  kubectl -n "$TEST_NS" get services -o yaml || true
}

phase create-cluster create_cluster
phase load-image load_image
phase deploy deploy
for p in leader_lease metrics services reconciles_ours skips_other_class no_restarts; do
  phase "${p//_/-}" "$p"
done
log "passed: $image"
