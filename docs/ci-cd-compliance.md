# CI/CD compliance — crusoe-load-balancer-controller

| Field | Value |
|---|---|
| Component / Service name | crusoe-load-balancer-controller |
| Team | Managed Orchestration |
| Reviewer | onboard-mo-cicd (Claude), reviewed by Safdar Iqbal |
| Review date | 2026-11-19 |

Scope of the onboarding: mo-cicd owns the merge-request gate and the `<sha>-dev` builds
on merge. Releases stay on GitHub (`.github/workflows/tag-release.yml` + goreleaser to
`ghcr.io/crusoecloud/crusoe-load-balancer-controller`). Items that depend on the MO
release engine are WIP for that reason.

## 1. Pipeline structure

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 1 | Small increments | WIP | No MR template in `.gitlab/` (only `CODEOWNERS`); MR size is not guided |
| 2 | Separate MRs per environment | WIP | Platform (engine tiered rollout). Today the version is pinned in `cmk-addon-catalog` |
| 3 | Release docs from commits + MRs | WIP | Releases come from goreleaser on GitHub with `changelog.disable: true` (`.goreleaser.yml`); mo-cicd release notes are not used |
| 4 | Tests shifted left to pre-merge | Done | mo-cicd `pre-merge:lint`, `pre-merge:tests` (`make ci`), dry-runs, `pre-merge:functional` |
| 5 | Ephemeral test environments | Done | `ci-tests/functional/test.sh`: Kind cluster per MR for the e2e suite (`test/e2e`); mo-cicd deletes it at the end |

## 2. Security & supply chain

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 6 | Tokens, not user/password | Done | mo-cicd job tokens; GitHub uses `GITHUB_TOKEN`; the hooks use no credentials |
| 7 | Artifacts signed before production | WIP | goreleaser GPG-signs the checksums file; container images are not signed |
| 8 | GCP keys as sensitive CI variables | Done | No GCP credentials in this pipeline |
| 9 | SA keys via cloud-terraform + devx/gitlab | Done | No service-account keys in this pipeline |
| 10 | SBOM of dependencies | WIP | Platform (post-MVP) |
| 11 | Security gates pre-merge | Done | mo-cicd `security:repo`, `security:images`; checkov clean with reasoned skips in `.checkov.yaml` |

## 3. Progressive delivery

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 12 | CI Dev → CI Stage → Crusoe Dev → Stage → Prod | WIP | Platform (engine tiered rollout) |
| 13 | Argo Rollouts | Done | SIG exception: leader-elected controller with no request traffic (CMKv2 RFC) |
| 14 | AnalysisTemplates | Done | Same exception as 13 |
| 15 | Automated rollback criteria | Done | Same exception as 13 |

## 4. GitOps

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 16 | App and deployment code in separate repos | Done | Chart in `crusoe-load-balancer-controller-helm-charts` (`charts/crusoe-lb-controller`) |
| 17 | Cluster state in YAML in Git | Done | `cmk-addon-catalog` pins chart `0.0.24` per Kubernetes minor |
| 18 | Operator (Argo CD) reconciles | WIP | Deployed by addon-controller from `cmk-addon-catalog`, not Argo CD; confirm this counts |
| 19 | Drift detection per environment | WIP | Platform |
| 20 | Zero-touch deploys | WIP | Platform (recorded deviation, CMKv2 RFC item 20) |
| 21 | Blue/green or canary | WIP | Platform |

## 5. Policy enforcement

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 22 | No advance until the current tier passes | WIP | The merge gate exists; "Pipelines must succeed" on `main` is a project setting to confirm. Tiers: platform |
| 23 | SBOM per release | WIP | Platform |
| 24 | 1-week soak per environment | WIP | Platform (tracked, not enforced) |
| 25 | Kyverno on k8s resources | WIP | The functional hook does not run the cell Kyverno policies |
| 26 | Gatekeeper for API governance | Done | SIG decision: Gatekeeper rejected for cells (Kyverno is the standard) |

## 6. Shift-left / pre-merge

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 27 | All non-env-specific tests pre-merge | Done | `make ci`, `ci-tests/pre-merge/test.sh`, `ci-tests/functional/test.sh` (`test/e2e`) |
| 28 | Lint | Done | golangci-lint v2.15.0 (`.golangci.yml`), shellcheck, hadolint, yamllint (`.yamllint`) |
| 29 | Unit tests | Done | `make test-ci`: envtest + gotestsum, `tests.xml` JUnit and coverage |
| 30 | Syntax | Done | mo-cicd `buildx --check` dry-run, yamllint |
| 31 | Static analysis | Done | golangci-lint (staticcheck, revive, …), mo-cicd Semgrep / Wiz SAST |
| 32 | Security / policy testing | Done | checkov (`.checkov.yaml`), Wiz IaC |
| 33 | Drift configuration testing | Done | `ci-tests/pre-merge/test.sh`: `go mod tidy` and `make manifests generate` drift |
| 34 | Vulnerability scanning | Done | mo-cicd trivy/grype/Wiz on images and dependencies |

## 7. Toolchain

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 35 | Open source preferred | Done | mo-cicd OSS fallbacks |
| 36 | Vendor tools justified | Done | Wiz (mo-cicd RFC) |
| 37 | Shared pipeline logic in a common repo | Done | `.gitlab-ci.yml` includes mo-cicd |
| 38 | Standard formatting enforced | Done | gofmt + goimports as golangci-lint formatters |

## 8. Observability & DORA

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 39 | Prod deployment traceable to commit | WIP | Git tag `v0.0.N` ↔ ghcr tag, but the chart defaults to `latest` and the deployed tag pin is not verified |
| 40 | Release version mapped to Jira tickets | WIP | Recent commits have no Jira keys |
| 41 | Pipeline success/failure to Crusoe metrics | WIP | Platform; GitLab keeps JUnit and logs |
| 42 | Pipeline execution time to Crusoe metrics | WIP | Platform |
| 43 | Failure logs to Crusoe logging | WIP | Platform |
| 44 | DORA: deployment frequency | WIP | Platform |
| 45 | DORA: lead time | WIP | Platform |
| 46 | DORA: change failure rate | WIP | Platform |
| 47 | DORA: MTTR | WIP | Platform |

## 9. Deprecation & migration

| # | Requirement | Status | Evidence |
|---|---|---|---|
| 48 | Migration docs on release day | WIP | No process in this repository |
| 49 | Backward compatibility 6 months | WIP | No stated policy (annotations are the user interface) |
| 50 | API versioning | WIP | No CRDs; the `crusoe.ai/*` Service annotations are not versioned |
| 51 | Deprecation announcements | WIP | No process in this repository |

## Summary

Done: 24/51 · WIP: 27/51 · covered by mo-cicd: 15 (items 4, 5, 6, 11, 27–32, 34–38)
