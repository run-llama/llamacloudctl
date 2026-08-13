# CLAUDE.md — agent guide for llamacloudctl

This file is for AI assistants (Claude Code and similar) helping a human install,
upgrade, or manage a **customer-managed LlamaCloud** deployment with this CLI.
The human you are helping is typically a devops engineer who may not know
Kubernetes or Helm deeply — the CLI is a fully guided wizard, and your job is to
help them through it, not around it.

## What this tool is

`llamacloudctl` installs and manages the `llamacloud` Helm chart
(pulled from `https://run-llama.github.io/helm-charts`) using an embedded Helm
SDK — it never shells out to `helm` or `kubectl`. Commands:

| Command | Purpose |
|---|---|
| `install` | Guided wizard: collects values, validates against the chart schema, shows a plan, installs, streams rollout progress. |
| `upgrade` | Diffs the target chart version's `values.schema.json` against the installed one, guides migration of breaking changes, re-validates, upgrades. |
| `preflight` | Read-only readiness checks: values, license, chart render, cluster version/capacity, database reachability, OIDC discovery. Exits non-zero on FAIL. |
| `permissions` | Checks the current AWS identity against the IAM actions each install step needs (live simulation); prints a least-privilege policy to request. Azure/GCP: lists required roles (no live simulation). |
| `terraform` | Emits self-contained Terraform (buckets + workload identity, optionally managed Postgres/Redis) for the customer's infra team. Makes no cloud calls. |
| `uninstall` | Removes the release and in-cluster DBs; `--delete-managed` also destroys managed cloud DBs (destructive). |
| `version` | Prints the version (stdout). |

## How to interact with the wizard as an agent

Pass `--plain` (any command) to force **plain accessible mode**: sequential
prompts, numbered menus, no ANSI screens. The wizard also drops to this mode on
its own when stdout is not a TTY or `NO_COLOR=1` is set. Two ways to drive it:

**Turn-by-turn (recommended for a first install)** — run it under tmux and
read/answer each prompt:

```bash
tmux new-session -d -s ctl -x 120 -y 40 "llamacloudctl install --plain --dry-run"
tmux capture-pane -t ctl -p          # read the current prompt
tmux send-keys -t ctl "2" Enter      # answer it (menus take a number)
```

Always read the pane before answering — never send a canned answer sequence
blind; prompt order can vary with earlier answers.

**Non-interactive replay (for repeat installs)** — a real install writes the
generated values to `<namespace>-values.yaml` (a `--dry-run` writes nothing
unless you pass `--output <path>`). Re-run the same install without prompts:

```bash
llamacloudctl install -f llamacloud-values.yaml --yes
```

**A values file records VALUES, not INTENT.** The chart schema sets
`additionalProperties: false`, so there is nowhere legal in the file to record
that the wizard was told to *create* the buckets (or to stand up the in-cluster
databases). Those live in flags, and a replay that omits them silently does LESS
than the original run:

| Original run chose | Replay must add |
| --- | --- |
| "create buckets for me" | `--create-buckets` (plus `--region <r>` if not the default) |
| in-cluster postgres/redis | `--provision-in-cluster --storage-class <class>` |

The install prints the exact replay command — including these flags — right
after `Saved values to …`. **Copy that line**; do not reconstruct it by hand.
Creating a bucket the account already owns is a no-op, so `--create-buckets` is
safe to keep on every replay of a create-for-me install.

This matters because of a real failure: a create-for-me run died before the
bucket step, was replayed as `-f saved-values.yaml --yes`, created no bucket, and
reported `status=deployed` against a bucket that did not exist. The first symptom
was a runtime upload failure. The install's values-phase preflight now HeadBuckets
every configured bucket it is not about to create and WARNs by name when one is
missing — read that warning, do not scroll past it.

`--dry-run` renders, validates, and shows the plan without installing — use it
before every real install.

## What the human must have ready (input checklist)

Collect these BEFORE starting the wizard. Never invent, guess, or fabricate any
of them — if the human doesn't have one, stop and tell them who to ask.

1. **Kubeconfig + context** for the target cluster (`--kubeconfig`, `--context`).
   The install targets whatever the current context points at — confirm it with
   the human before proceeding.
2. **A LlamaCloud license JWT.** The wizard offers two sources: paste the JWT,
   or reference an existing Kubernetes Secret by name. **Prefer the Secret
   reference** — it keeps the JWT out of shell history and terminal logs. If
   they have the raw JWT, help them create the Secret first.
3. **Authentication method — basic or OIDC** (the wizard always asks, right
   after the license; there is no default).
   - **basic** — an email *domain* sign-ups must use, plus a JWT signing secret
     (any long random string). Two constraints the installer does NOT validate
     but registration enforces downstream — so an install can reach
     `status=deployed` yet let no one sign in: the domain must be one that
     **actually accepts mail** (a placeholder like `example.com` is rejected,
     "domain does not accept email"), and the first user's password must be **at
     least 16 characters**. Give a real domain the customer controls.
   - **oidc** — the identity provider's discovery URL, client ID, and client
     secret. Over a port-forward, register `http://localhost:8080/oidc/callback`
     as a redirect URI first, or sign-in fails.
4. **At least one LLM provider credential.** `openai` / `anthropic` / `gemini`
   take a plain API key. The three enterprise providers have structured config
   and ask follow-up questions:
   - `bedrock` — an AWS **region** (required), then how to authenticate:
     **IAM role (the default, recommended on EKS)** needs *no credentials at
     all* (the pods use IRSA or the node role — just grant that role
     `bedrock:InvokeModel`), or a static access key + secret, or the name of a
     Kubernetes Secret they already created. Do NOT tell the human to
     hand-create an empty Secret: an empty Secret leaves Bedrock disabled.
   - `vertex` — GCP project + location + the service-account key JSON (this
     chart has no credentials-free Vertex mode), or an existing Secret name.
   - `azure_openai` — one deployment (model, API key, base URL, API version;
     deployment name optional), or an existing Secret name. Extra deployments
     are added by editing `config.llms.azureOpenAi.deployments` in the saved
     values file.
5. **Namespace** (default `llamacloud`) and **release name** (default `llamacloud`).
6. **Target cloud** (`--cloud aws|azure|gcp`, default aws) — drives object
   storage and workload-identity wiring.
7. **Object-storage decision.** On AWS the wizard asks *create buckets for me*
   vs *use existing buckets*; on gcp/azure the bucket must ALREADY EXIST (the
   installer creates buckets on AWS only — everything else rides the s3proxy
   shim against an endpoint/identity/credential the operator supplies).
   - *create for me* — needs `s3:CreateBucket` and a bucket name that is free
     GLOBALLY across all of AWS. On a non-interactive run this is the
     `--create-buckets` flag, never a values-file key.
   - *use existing* — the human must confirm the bucket EXISTS in the account
     the platform's identity uses. Do not accept a name on trust: the values
     preflight HeadBuckets it, and a WARNING naming the bucket means uploads and
     parsing will fail at runtime once installed.
8. **Data-service decisions** for PostgreSQL and Redis, one of:
   - *existing endpoint* — host/port/credentials of databases they already run;
   - *install in-cluster* — simplest, no cloud setup, uses persistent volumes;
   - *provision managed* — the CLI creates cloud DBs (RDS/ElastiCache,
     Cloud SQL/Memorystore, Flexible Server/Azure Managed Redis). Requires the
     networking inputs below and cloud permissions;
   - *terraform handoff* — no cloud permissions: emits a Terraform bundle for
     the infra team and stops (see the round-trip below).

   **If either service goes in-cluster, a StorageClass matters.** The wizard
   asks once (both paths, minimal and customize) and `--storage-class` prefills
   it. On a cluster with **no default StorageClass** — common on EKS — the
   preflight says so and the wizard then REFUSES an empty answer, because a PVC
   that names no class stays Pending and the install times out. Get the value
   from `kubectl get storageclass` (typically `gp2` or `gp3` on EKS); never
   invent one.
9. **If provisioning managed data services** — the CLI creates **zero
   networking**; the operator must supply EXISTING identifiers:
   - AWS: `--db-subnet-group-name`, `--db-security-group-ids` (allow 5432 from
     the cluster), `--cache-subnet-group-name`, `--cache-security-group-ids`
     (allow 6379), `--region`.
   - GCP: `--gcp-project`, `--gcp-vpc-network` (must already have a
     private-services-access connection), `--gcp-allocated-ip-range` (a range
     NAME, not a CIDR), `--region`. Never propose creating a new GCP project.
   - Azure: `--azure-subscription-id`, `--azure-resource-group`,
     `--pg-delegated-subnet-id` (existing subnet with the
     `Microsoft.DBforPostgreSQL/flexibleServers` delegation),
     `--pg-private-dns-zone-id` (`*.postgres.database.azure.com` zone),
     `--redis-private-endpoint-id`, `--region` (REQUIRED — no env fallback
     off AWS).
   These come from the customer's infra team. If the human doesn't know them,
   route to the terraform handoff instead of guessing.

## Recommended flow

```
preflight  →  permissions (if cloud resources will be created)  →  install --dry-run  →  install
```

1. `llamacloudctl preflight` — read-only; fix FAILs before anything else. Each
   failure prints a Remediation line — relay it to the human verbatim.
2. `llamacloudctl permissions --managed postgres,redis` when the install will
   provision managed DBs (managed steps are NOT checked without `--managed`).
   Add `--teardown` if they'll also want uninstall coverage, and
   `--create-buckets=false` when pointing at existing buckets. If DENIED, the
   printed IAM policy is what the human hands their administrator — or use the
   terraform handoff.
3. `llamacloudctl install --dry-run` — walk the wizard and review the plan with
   the human; nothing is installed and no values file is written (pass
   `--output <path>` to save the previewed values). Step 4's real install writes
   the reusable values file.
4. `llamacloudctl install` (or `install -f <values> --yes` to replay). Rollout
   progress streams live; on failure it prints per-pod diagnostics
   (ImagePullBackOff, unready pods) and leaves resources in place for diagnosis.

The install auto-runs trimmed preflight phases itself (environment checks
before the wizard, values + render checks after); `--skip-preflight` and
`--skip-permission-check` exist but don't reach for them to make errors go
away — fix the underlying issue.

## The no-permissions path (terraform round-trip)

When the human lacks cloud permissions for buckets/identity/managed DBs:

```bash
llamacloudctl terraform --managed postgres,redis --out-dir ./llamacloud-tf -f <values>
# → hand ./llamacloud-tf to the infra team; they: terraform init && apply,
#   then send back: terraform output -json > outputs.json
llamacloudctl install --from-terraform outputs.json --yes
```

The bundle consumes existing networking as variables and creates none. On
azure/gcp, `--from-terraform` alone is not sufficient — also pass `-f` values
enabling `config.storageBuckets.s3proxy` with the JCLOUDS endpoint/credentials
(the output contract carries no object-store credential).

Adding `--provision-in-cluster` to a `--from-terraform` run is the supported way
to get in-cluster **Temporal** non-interactively. The flag is scoped to services
the values do not already give an endpoint for, so the terraform-supplied
PostgreSQL/Redis are kept (the run prints which source won for each) and only
Temporal is deployed in-cluster, wired to that PostgreSQL with TLS on. It does
NOT override supplied endpoints — if you see the plan report
`postgresql: in-cluster` after `--from-terraform`, something is wrong; read the
per-service notices in the run output.

## Upgrades

```bash
llamacloudctl upgrade --chart-version <target>          # guided migration
llamacloudctl upgrade --chart-version <target> --dry-run # diff + validate only
```

Breaking changes in the values the customer actually set are flagged and
migrated interactively; the result is re-validated against the target schema
before the upgrade runs. `--yes` with unresolved breaking changes fails fast by
design (it will not silently proceed). `--json` emits a machine-readable report
and never prompts. The migrated values are saved (`--save`, default
`llamacloud-values.yaml`) — tell the human to keep that file.

## Safety rules for agents

- **Never fabricate credentials, endpoints, subnet IDs, or license tokens.**
  Every such value comes from the human or their infra team.
- **Prefer Secret references over pasting secrets** anywhere a prompt offers it.
- **Always `--dry-run` first** and review the plan with the human before a real
  install.
- **`uninstall --delete-managed` destroys cloud databases and skips final
  snapshots.** Confirm explicitly with the human, restate what will be deleted,
  and never pass `--yes` to it on their behalf without that confirmation.
- The same-account guard refuses installs whose buckets would land in a
  different AWS account than the cluster; `--allow-cross-account` overrides it —
  only with the human's explicit confirmation that this is intended.
- Exit codes are meaningful: `preflight` exits non-zero on FAIL and is safe to
  gate CI on. `--json` output is report-only and parseable.
- Rendered values echo masks credentials (`***REDACTED***`); do not work around
  that masking when showing output to the human.

## Verifying an install

After `install` reports `Release "llamacloud" installed: status=deployed`:

```bash
kubectl -n <namespace> get pods        # all Running/Ready
llamacloudctl preflight                # re-check against the live values
```

If rollout timed out, the streamed output already names the failing pods and
reasons — start there, not with generic cluster spelunking.

## Troubleshooting

Read what the CLI printed before running anything by hand — every case below is
already named in its output.

**In-cluster database never becomes ready / "context deadline exceeded".**
The install streams per-release progress while it waits (`... postgresql: 0/1
pods ready`) and, on failure, prints a `Diagnostics for in-cluster release`
block listing the PVCs (phase + `storageClassName`), the not-ready pods with
their reason, and the cluster's own `FailedScheduling` events. The error itself
names the failing resource. The overwhelmingly common cause is a PVC that is
`Pending` with `storageClassName: none` — re-run with `--storage-class <class>`.

**Preflight WARNs that a bucket does NOT exist.** This is a WARNING, not a FAIL,
so it will not stop the install — and that is exactly why it must not be scrolled
past. The platform will come up `status=deployed` and the first document upload
will fail. Either create the bucket, point `config.storageBuckets` at one that
exists, or re-run with `--create-buckets`. A `Skipped the object-storage existence
check` line means no AWS credentials were available — nobody looked, so the bucket
is still unverified; check it by hand.

**"refusing to install in-cluster … PersistentVolumeClaim … would be adopted".**
A claim from an earlier install survived. A StatefulSet **adopts** a matching
existing claim unchanged, so re-running with the correct `--storage-class` does
NOT fix a claim that was born without one. Interactively the CLI offers to
delete the claim (and the pod holding it) — that DELETES the data on it, so
confirm with the human first. Non-interactively it fails fast and prints the
exact `kubectl delete pvc` command.

**After `uninstall`, the next install misbehaves.** `uninstall` does not delete
PVCs; its completion output lists what was left behind with each claim's phase
and class, plus the `kubectl delete pvc` command. Re-installing reuses those
volumes.

**`llamacloud-parse` shows 0 replicas / no pod.** That is intentional, not a
failure. `llamacloud-parse` is the **v1** parse worker and its only work channel is
the RabbitMQ queue, which this installer disables along with mongodb and qdrant —
so the pod could never pass readiness and would stall the rollout wait until the
release reported `status=failed`. Every generated values file therefore sets
`llamaParse.replicas: 0`, and the install plan says so before you approve it.
**Parsing still works**: it runs through the Temporal parse path
(`llamacloud-temporal-parse`), which is untouched. Do not "fix" this by raising
the replica count — that reintroduces the stalled rollout. If a values file you
were handed sets a nonzero `llamaParse.replicas`, the installer keeps it and the
values preflight warns; set it to 0 unless you are also enabling rabbitmq.

**The Temporal schema job crash-loops: "no pg_hba.conf entry … no encryption".**
The `llamacloud-temporal-subchart-schema-*` pod is a Helm **pre-install hook**, so
this one failure blocks every other workload and the rollout only ever tracks
that single pod. The cause is a Temporal datastore pointed at a PostgreSQL that
enforces TLS (any managed instance — RDS defaults to `rds.force_ssl=1`) with the
subchart's SQL TLS off. Current versions wire it automatically for every non
in-cluster PostgreSQL; a values file predating that, or one that pins
`sql.tls.enabled: false`, reproduces it. Fix by setting, for BOTH the `default`
and `visibility` datastores:
`temporal-subchart.server.config.persistence.datastores.<store>.sql.tls.enabled: true`
plus `sql.tls.enableHostVerification: false` (the AWS RDS CA is not in the pod
trust store, so verification would fail the connection). The values preflight
warns about this combination before the install — check `temporal.datastore.tls`
in the report.

**Managed cloud database seems stuck.** The CLI prints a heartbeat naming the
resource and the elapsed time (`... still creating RDS PostgreSQL "…"`). Managed
creates commonly take 5–15 minutes; that is not a hang.

**Bedrock configured but the platform does not use it.** Check that
`config.llms.awsBedrock.region` is set — the chart renders no Bedrock
configuration without it. If the human referenced an existing Secret, that
Secret must actually contain the `AWS_BEDROCK_*` variables; an empty one leaves
Bedrock off. The IAM-role mode (region only, no credentials) is the correct EKS
default.
