#!/usr/bin/env bash
#
# Local end-to-end test against a throwaway kind cluster.
#
# This is the developer-machine entry point to the kind end-to-end suite. It
# proves the install path works against a REAL Kubernetes API server —
# internal/kube, the embedded
# Helm SDK install, in-cluster data-service provisioning, rollout streaming and
# its failure path, and uninstall — without needing a cloud account, a cluster,
# or a license.
#
# What it runs is the Go suite in internal/e2e, which owns the cluster lifecycle:
# it creates (or reuses) a kind cluster named llamacloudctl-e2e, runs the tests,
# and deletes the cluster again.
#
# SAFETY — this script and the suite it runs never touch your default kubeconfig.
# The kind cluster is created with an explicit --kubeconfig under
# internal/e2e/testdata/tmp/ (gitignored); KUBECONFIG is repointed at that file
# for the test process, every call into install/uninstall is passed the path
# explicitly, and the harness refuses to run unless that kubeconfig contains
# exactly one context, named kind-llamacloudctl-e2e, whose API server is on
# loopback. The cloud credential environment is scrubbed (AWS config/credential
# files -> /dev/null, IMDS disabled, keys and profile emptied) so the suite
# cannot reach a cloud account even by accident.
#
# Usage:
#   scripts/e2e-kind.sh                 # create cluster, run suite, delete cluster
#   scripts/e2e-kind.sh --keep          # leave the cluster up for the next run
#   scripts/e2e-kind.sh --fresh         # delete any existing cluster first
#   scripts/e2e-kind.sh --clean         # just force-delete a leftover cluster and exit
#   scripts/e2e-kind.sh -run TestE2EKube  # any extra args go to `go test`
#
# Requirements: Docker running, and kind on PATH (brew install kind).

set -euo pipefail

CLUSTER_NAME="llamacloudctl-e2e"
KUBECONFIG_PATH="internal/e2e/testdata/tmp/kubeconfig"
TIMEOUT="${E2E_KIND_TIMEOUT:-40m}"

# Keep this identical to TAGS in the Makefile: the troubleshoot analyzers the
# preflight embeds need them to build.
TAGS="netgo containers_image_ostree_stub exclude_graphdriver_devicemapper exclude_graphdriver_btrfs containers_image_openpgp"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

# Always run from the repository root so the relative paths above resolve.
cd "$(dirname "$0")/.."

CLEAN_ONLY=false
GO_TEST_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --keep)  export LLAMACLOUDCTL_E2E_KEEP=1 ;;
    --fresh) export LLAMACLOUDCTL_E2E_FRESH=1 ;;
    --clean) CLEAN_ONLY=true ;;
    *)       GO_TEST_ARGS+=("$arg") ;;
  esac
done

command -v kind >/dev/null 2>&1 || fail "kind is not installed — brew install kind"
docker info >/dev/null 2>&1 || fail "Docker does not appear to be running"

if [ "$CLEAN_ONLY" = true ]; then
  log "force-deleting kind cluster $CLUSTER_NAME (and its dedicated kubeconfig)"
  kind delete cluster --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_PATH" || true
  rm -f "$KUBECONFIG_PATH"
  exit 0
fi

# The suite sets these itself (internal/e2e/harness_test.go scrubCloudEnvironment),
# but exporting them here too means even a `go test` that fails before TestMain
# runs — or anything else this shell spawns — is already sandboxed.
export KUBECONFIG="$PWD/$KUBECONFIG_PATH"
export AWS_CONFIG_FILE=/dev/null
export AWS_SHARED_CREDENTIALS_FILE=/dev/null
export AWS_EC2_METADATA_DISABLED=true
export AWS_ACCESS_KEY_ID=
export AWS_SECRET_ACCESS_KEY=
export AWS_SESSION_TOKEN=
export AWS_PROFILE=
export AWS_DEFAULT_PROFILE=

export LLAMACLOUDCTL_E2E=1

log "running the kind end-to-end suite (cluster $CLUSTER_NAME, kubeconfig $KUBECONFIG_PATH)"
go test -tags "$TAGS" ./internal/e2e/... -v -count=1 -timeout "$TIMEOUT" ${GO_TEST_ARGS[@]+"${GO_TEST_ARGS[@]}"}
