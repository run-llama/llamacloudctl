BINARY := bin/llamacloudctl
PKG := github.com/run-llama/llamacloudctl
CMD := ./cmd/llamacloudctl
VERSION ?= 0.0.0-dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(PKG)/internal/cli.Version=$(VERSION) \
	-X $(PKG)/internal/cli.Commit=$(COMMIT) \
	-X $(PKG)/internal/cli.Date=$(DATE)

## Build tags required by the embedded replicatedhq/troubleshoot analyzers. The
## containers/image and containers/storage packages troubleshoot pulls in default
## to build variants that need cgo and OS-specific storage drivers; these five tags
## select the pure-Go stubs instead, which is what keeps CGO_ENABLED=0 cross-builds
## working for every release target. Keep this list identical to the -tags entry in
## .goreleaser.yaml and the build-tags list in .golangci.yml.
TAGS ?= netgo containers_image_ostree_stub exclude_graphdriver_devicemapper exclude_graphdriver_btrfs containers_image_openpgp

.PHONY: build test golden vet fmt install clean e2e-kind e2e-kind-clean

## build: compile the CLI to ./bin/llamacloudctl with the version injected via ldflags
build:
	go build -tags "$(TAGS)" -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

## test: run all tests (Tier 1 unit + Tier 2 golden-manifest render). The render
## suite pulls the pinned llamacloud chart into a gitignored cache on first run
## (network needed once, cached thereafter); it skips gracefully when offline
## with no cache. No cluster or secrets required.
test:
	go test -tags "$(TAGS)" ./...

## e2e-kind: run the Tier-3 end-to-end suite against a throwaway kind cluster
## (internal/e2e). Creates the cluster, runs the real install/rollout/uninstall
## path against a live API server, then deletes the cluster. Needs Docker running
## and kind installed (brew install kind); no cloud account, cluster, or license.
## It NEVER touches your default kubeconfig — see scripts/e2e-kind.sh for the
## safety rails. Pass ARGS='--keep' to leave the cluster up between runs.
e2e-kind:
	./scripts/e2e-kind.sh $(ARGS)

## e2e-kind-clean: force-delete a leftover llamacloudctl-e2e kind cluster (and its
## dedicated kubeconfig) after an interrupted run.
e2e-kind-clean:
	./scripts/e2e-kind.sh --clean

## golden: regenerate the Tier-2 golden manifests after a deliberate chart-version
## bump or seed change. Review the diff before committing.
golden:
	go test -tags "$(TAGS)" ./internal/render/... -update

## vet: run go vet
vet:
	go vet -tags "$(TAGS)" ./...

## fmt: gofmt the tree
fmt:
	gofmt -w .

## install: install the binary onto GOBIN / $(go env GOPATH)/bin
install:
	go install -tags "$(TAGS)" -ldflags "$(LDFLAGS)" $(CMD)

## clean: remove build artifacts
clean:
	rm -rf bin
