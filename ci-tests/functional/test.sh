#!/usr/bin/env bash
# functional hook: mo-cicd runs it as pre-merge:functional on every merge request,
# with a Docker daemon (dind) and docker/kind/kubectl/go on PATH.
#
# It runs the kubebuilder e2e suite (test/e2e) on a new Kind cluster, with this MR's
# image (E2E_IMAGE). mo-cicd deletes every Kind cluster when the hook ends.
# Locally, use `make test-e2e` against your own Kind cluster.
#
# Provided by the job (mo-cicd scripts/run-functional.sh):
#   MO_IMAGES         this pipeline's images, pushed as <name>:<sha> by pre-merge:images
#   MO_KIND_CLUSTER   a unique cluster name
#   MO_REPORTS_DIR    JUnit files dropped here (junit-functional-*.xml) show on the MR
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

reports="${MO_REPORTS_DIR:-$PWD}"
mkdir -p "$reports"
reports="$(cd "$reports" && pwd)"   # ginkgo resolves a relative path from test/e2e

# This MR's controller image, which pre-merge:images pushed as <name>:<sha>.
image=""
for ref in ${MO_IMAGES//,/ }; do
  [[ "${ref%:*}" == */crusoe-load-balancer-controller ]] && image="$ref"
done
[[ -n "$image" ]] || { echo "controller image not in MO_IMAGES (${MO_IMAGES:-})" >&2; exit 1; }
# kind load reads the local daemon. mo-cicd has already logged in to the registry.
docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"

kind create cluster --name "${MO_KIND_CLUSTER:?MO_KIND_CLUSTER is not set}" --wait 2m
# Nothing in config/default uses CertManager (no webhooks or certificates).
KIND_CLUSTER="$MO_KIND_CLUSTER" CERT_MANAGER_INSTALL_SKIP=true E2E_IMAGE="$image" \
  go test ./test/e2e/ -v -ginkgo.v -timeout 30m \
  -ginkgo.junit-report="$reports/junit-functional-e2e.xml"
