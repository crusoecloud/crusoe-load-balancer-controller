#!/usr/bin/env bash
# pre-merge hook — runs in mo-cicd's pre-merge:tests job (image: $MO_CI_IMAGE) on
# every merge request, after the shared tests (`make ci`: envtest unit tests +
# golangci-lint; shellcheck, hadolint, yamllint). What the old `make test-ci` did on
# top of the tests lives here. Each check prints a heading and a one-line fix hint.
set -euo pipefail

log() { printf '== %s\n' "$*"; }

log "go.mod / go.sum are tidy"
go mod tidy
git diff --exit-code --stat -- go.mod go.sum \
  || { echo "run 'go mod tidy' and commit the result" >&2; exit 1; }

log "generated RBAC and manifests are up to date"
make manifests generate
git diff --exit-code --stat -- config/ internal/ cmd/ \
  || { echo "run 'make manifests generate' and commit the result" >&2; exit 1; }
