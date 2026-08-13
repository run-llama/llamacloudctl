# Tier-2 render harness (`internal/render`)

This package proves that the values llamacloudctl assembles render into correct,
schema-valid Kubernetes manifests against the **real, pinned** `llamacloud` Helm
chart — with **no cluster, no license, and no LLM keys**. It is Tier 2 of the
test fidelity ladder:

| Tier | What it proves | Where |
| ---- | -------------- | ----- |
| 1 | Pure value-assembly logic (guards, wizard mapping, redaction, flags) | unit tests across `internal/*` |
| **2** | **Assembled values → schema-valid, correct manifests against the pinned real chart** | **this package** |
| 3 | A real cluster install + upgrade actually comes up | [`internal/e2e`](../e2e/README.md) (kind only). The published-chart real converge, the last-GA released-binary transition, and cloud provisioning are EKS-only and no longer covered anywhere in CI under the no-real-cluster rule (see the e2e README) |

## How it works

For each **profile** in the matrix (`matrix.go`), `TestRenderProfiles`:

1. **Assembles values** one of two ways:
   - **Seed profiles** coalesce seed file(s) with Helm's own `-f` layering
     (`values.Load`) — what a scripted `install --values … --non-interactive` run
     does (the wizard is bypassed). Proves the *chart* renders from given values.
   - **Wizard profiles** (`WizardAnswers`) drive the **real** `install.RunWizard`
     headlessly through a scripted answers fixture. Proves the *wizard's value
     mapping* is chart-valid end to end — this is what catches wizard→chart key
     bugs (seed profiles cannot, since they hand-write chart-valid keys).
2. **Runs the install guards** (`install.Validate`). A *rejection* profile stops
   here and asserts the expected guard FAILs. A *render* profile requires the
   guards to pass.
3. **Schema-validates** the assembled values against the chart's bundled
   `values.schema.json` (`install.ValidateValues`).
4. **Renders** the chart to its full manifest via Helm's install action in
   `ClientOnly + DryRun` mode — the `helm template` equivalent, entirely
   client-side (a mock kube client, in-memory storage, default capabilities). The
   output includes lifecycle **hooks** (e.g. the DB-migration Job).
5. **Compares byte-for-byte** against the committed golden in `testdata/golden/`.

Full-manifest goldens are safe because the chart version is **pinned**
(`PinnedChartVersion` in `render.go`): a golden only churns on a deliberate
version bump, never incidentally — which makes the snapshot maximally sensitive
to any change in what the installer would ship.

### Non-deterministic values

The chart emits exactly one per-render random value — the OIDC secret's
`SESSION_SECRET_KEY` (`randAlphaNum`, confirmed the sole random/uuid/time-based
value in 0.8.5). `render.Normalize` masks it to a stable placeholder before every
golden write and compare, so goldens stay byte-stable. If a future chart version
introduces other volatile values, extend `Normalize` (and this note) rather than
abandoning byte-wise comparison.

## The pinned chart: pull + cache (never committed)

The pinned chart is pulled once from the public repo
(`https://run-llama.github.io/helm-charts`) into `testdata/chart-cache/`, which is
**gitignored**. Every subsequent run loads the cached archive **offline**. Only
the golden *output* is committed — never the chart bytes.

- **First run needs network** to warm the cache; cached thereafter.
- If the chart is neither cached nor fetchable (a genuinely offline first run),
  the render profiles **skip** with a message telling you to warm the cache. The
  rejection profiles still run (they need no chart). No secrets are ever
  required.
- **CI**: run `go test ./internal/render/...` with network available once (or
  pre-warm `testdata/chart-cache/`); no secrets needed.

## Regenerating goldens (`-update`)

After a deliberate chart-version bump or an intended change to a profile's seed:

```bash
go test ./internal/render/... -update
```

Run `-update` **only** against this package — other packages do not define the
`-update` flag. Then review the golden diff (`git diff internal/render/testdata/golden`)
before committing.

## Adding a profile to the matrix

The matrix is designed so a future ticket plugs in its own coverage without
touching a monolith:

1. Add the profile's input, one of:
   - **Seed** file(s) under `testdata/profiles/` (reuse `base-existing.yaml` as a
     shared substrate + a thin overlay, or a standalone seed), or
   - a **wizard-answers** fixture under `testdata/wizard/` (keyed by prompt
     title) to drive the real wizard.
2. Append a `Profile` to `Profiles` in `matrix.go`, grouped by `Capability`:
   - **Render profile** — leave `ExpectGuardFail` empty; set `Seeds` **or**
     `WizardAnswers`. It must pass guards, schema-validate, and match a golden.
   - **Rejection profile** — set `ExpectGuardFail` to the guard-check name that
     must FAIL (e.g. `"LLM providers"`). It renders nothing and has no golden.
3. Generate its golden: `go test ./internal/render/... -update`.
4. Commit the input fixture(s) + golden. The harness iterates whatever `Profiles`
   contains — no harness code changes.

### Current profiles

| Profile | Capability | Source | Kind | Proves |
| ------- | ---------- | ------ | ---- | ------ |
| `aws-existing` | core | seed | render | AWS object storage + all data services as existing endpoints (the demoable core path) |
| `minimal` | core | seed | render | smallest valid install: pg + redis only, mongo/rabbitmq/temporal off |
| `azure-s3proxy` | storage | seed | render | non-AWS storage renders the s3proxy shim |
| `wizard-basic-auth` | auth | wizard | render | the real wizard's output with basic auth is chart-valid end to end |
| `wizard-oidc-auth` | auth | wizard | render | the real wizard's output with OIDC auth is chart-valid (guards the `discoveryUrl`/`enabled` fix) |
| `anthropic-embeddings-guard` | guards | seed | rejection | an Anthropic-only LLM config is rejected before render (no embeddings model) |

> The two `wizard-*` profiles are what surfaced (and now permanently guard
> against) the OIDC key-mapping bug and the uninstallable `none` auth mode.
