# Tier-3 kind end-to-end harness (`internal/e2e`)

This package proves that the install path llamacloudctl ships actually works
against a **real Kubernetes API server** — not a fake clientset, not a rendered
manifest. It is Tier 3 of the test fidelity ladder (see
[`internal/render/README.md`](../render/README.md) for tiers 1 and 2), on the
cheapest cluster that can prove it: a throwaway [kind](https://kind.sigs.k8s.io)
cluster on the developer's own Docker. No cloud account, no EKS cluster, no
license.

```bash
brew install kind     # one-time; Docker must be running
make e2e-kind         # create cluster ▸ run suite ▸ delete cluster
```

`make e2e-kind ARGS=--keep` leaves the cluster up (the next run reuses it),
`ARGS=--fresh` forces a clean slate, and `make e2e-kind-clean` force-deletes a
leftover cluster after an interrupted run. The suite is gated on
`LLAMACLOUDCTL_E2E` — mirroring the `LLAMACLOUDCTL_INTEGRATION` convention in
`internal/upgrade` — so a plain `go test ./...` compiles it and skips.

## Safety

The machine running this almost certainly has a default kubeconfig pointing at
production clusters. The harness never reads or writes it. Every run creates the
kind cluster with an explicit `--kubeconfig` under this package's gitignored
`testdata/tmp/`, repoints `KUBECONFIG` at that file, passes the path explicitly
on every call into `install`/`uninstall`, scrubs the cloud credential
environment, and refuses to start unless that kubeconfig holds **exactly one**
context — named `kind-llamacloudctl-e2e`, with its API server on loopback.
`uninstall` is therefore only ever reachable against the kind cluster.

## What each test proves

| Test | Exercised live |
| ---- | -------------- |
| `TestE2EKubeBootstrapAgainstKind` | `internal/kube`: `NewClientset`, `NewRESTConfig`, `List` / `Get` / `Status` against real Helm release storage |
| `TestE2EInstallInClusterConvergesThenUninstalls` | `install.Run` non-interactive → both preflight phases → `installInClusterDBs` (two real bitnami releases) → `runInstall` (embedded Helm SDK) + `streamRollout`; then `uninstall.Run` and post-teardown verification |
| `TestE2EInstallRolloutFailureSurfacesDiagnostics` | the rollout **failure** path: unpullable image + short timeout → streamed problem lines, `diagnoseNotReady`, and the resources-left-in-place error |
| `TestE2EPublishedChartDryRunAgainstKind` | the **published** LlamaCloud chart pulled from the real repo, schema-validated and rendered, with the preflight run against the live cluster |
| `TestE2EUpgradeConvergesAgainstKind` | the REAL Helm **upgrade** transaction + rollout wait: install the probe chart at 0.1.0, then `upgrade.Run` (via a `LocalFetcher`, no network) to a bumped 0.2.0 copy carrying a benign pod-template delta, asserting deployed revision 2 and a Ready rolled workload against the carried-forward in-cluster PostgreSQL/Redis |

The converging workload is `testdata/converge-chart`, passed to the install as
`--chart`. It stands in for the LlamaCloud chart so the real install flow runs
unchanged while installing something that can actually converge on kind using
public images only. Its pods gate readiness on reaching the in-cluster
PostgreSQL and Redis at the endpoints and credentials `internal/install`'s recipe
wired in, so a green rollout is evidence about the CLI's own wiring.

## What this does NOT prove

A full real install of the **published LlamaCloud chart** cannot converge on
kind: its images are private (they need a customer pull secret) and the app needs
a valid license plus reachable model providers, so its pods could never reach
Ready. Here that chart is taken as far as it can go — fetched, schema-validated,
rendered, dry-run against the live cluster.

The real **upgrade transaction** on a live API server IS covered on kind, by
`TestE2EUpgradeConvergesAgainstKind` (install the probe chart, then `upgrade.Run`
to a bumped local copy). What kind still cannot cover — EKS-only, and no longer
exercised anywhere in CI, which never reaches a real cluster or cloud — is: the published
LlamaCloud chart actually **converging** (whether on install or on upgrade), the
last-GA **released-binary → candidate** transition, and anything cloud-specific
(managed RDS/ElastiCache provisioning, S3 buckets, IAM). These are an accepted
residual gap of the kind-only move, not a chosen deferral: they require a real
cluster and cloud account, which CI is forbidden from touching.
