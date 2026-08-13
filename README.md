# llamacloudctl

`llamacloudctl` installs and manages a customer-managed LlamaCloud / LlamaParse
deployment on Kubernetes. It is a single static Go binary that embeds the Helm SDK
and client-go, so it never shells out to `helm` or `kubectl`. You run it anywhere
`kubectl` can reach your cluster; it installs the `llamacloud` chart,
provisions the data layer, and guides you through upgrades.

You keep control of the infrastructure, networking, data, and security. LlamaIndex
does not operate anything inside your account.

## Who this is for

Platform and infrastructure engineers standing LlamaParse up in their own cluster.
You do not need to know Helm or write a `values.yaml` by hand. The `install`
command is a wizard that asks questions and writes the values file for you. Managed
database and object-storage provisioning works on AWS, GCP, and Azure.

## Install the CLI

Prebuilt binaries are published for Linux, macOS, and Windows on `amd64` and
`arm64`. No runtime, no shared libraries.

```sh
sh -c "$(curl -fsSL https://raw.githubusercontent.com/run-llama/llamacloudctl/main/install.sh)"
```

The script detects your OS and CPU, downloads the matching release, verifies its
SHA-256 checksum, and installs to `/usr/local/bin` (or `~/.local/bin` when that is
not writable). Override the defaults with `LLAMACLOUDCTL_VERSION`,
`LLAMACLOUDCTL_INSTALL_DIR`, or `LLAMACLOUDCTL_REPO`.

Other ways to get it:

- **Linux packages**: `.deb`, `.rpm`, and `.apk` are attached to every release.
- **Manual download**: grab the archive for your platform from the
  [Releases page](https://github.com/run-llama/llamacloudctl/releases) and move the
  binary onto your `PATH`.
- **Verify authenticity**: release checksums are signed with cosign keyless
  (Sigstore + GitHub OIDC). Verify `checksums.txt.sig` before running in production.

## Step-by-step guides

Start here. Each guide walks a full managed install on that cloud, including the
networking you must create first, the exact flags, and teardown.

- [Deploy on AWS (EKS)](docs/aws.md): RDS PostgreSQL, ElastiCache Redis, S3.
- [Deploy on GCP (GKE)](docs/gcp.md): Cloud SQL, Memorystore, GCS.
- [Deploy on Azure (AKS)](docs/azure.md): PostgreSQL Flexible Server, Azure Managed
  Redis, Blob Storage.

For in-cluster databases instead of managed cloud services, any guide works. Pick
"install in-cluster" at the data-service prompts and skip the networking
prerequisites.

## The short version

Point `kubectl` at your cluster, then:

```sh
llamacloudctl preflight                 # read-only readiness check
llamacloudctl permissions --managed all # do I have the cloud access to provision?
llamacloudctl install -n llamacloud     # the guided wizard
```

`install` collects values through prompts, validates them against the chart's
`values.schema.json`, shows a plan, installs the chart, and streams the rollout. It
saves the generated `values.yaml` so you can reproduce the install non-interactively
with `install -f <saved-values.yaml> --yes`. Credentials are redacted in the
terminal but written in full to that file, so store it like a secret.

When the rollout finishes, confirm the platform actually works by parsing a document,
not just by checking pod health. Each per-cloud guide ends with a `scripts/parse-smoke.sh`
step that uploads a PDF and reads back the markdown, which exercises object storage,
the parse pipeline, Temporal, and the database together.

Have these ready before you start:

- **A cluster**, with `kubectl` pointed at it.
- **A license JWT**: paste it, or reference a Kubernetes Secret that holds it.
  Prefer the Secret; a pasted key lands in your shell history.
- **An embeddings-capable LLM key**: OpenAI, Azure OpenAI, Bedrock, Vertex, or
  Gemini. Anthropic alone cannot embed, so the wizard asks you to add one.
- **Object storage**: an existing bucket, or let the installer create one on AWS.
- **A StorageClass** for in-cluster databases if your cluster has no default (common
  on EKS). The wizard prompts for it; `--storage-class gp2` prefills the answer.
- **Existing networking**, only if you want managed cloud databases. The installer
  creates none of it. The per-cloud guide lists exactly what to create.

## Commands

| Command | What it does |
| --- | --- |
| `install` | Guided wizard: collect values, validate, plan, install, stream rollout. |
| `preflight` | Read-only readiness checks (cluster, license, chart render, database reachability). Exits non-zero on failure. |
| `permissions` | Check your cloud identity against the actions each install step needs, and print a policy to request if any are missing. |
| `terraform` | Emit self-contained Terraform for the cloud resources (buckets, identity, optional managed databases) so an infra team can apply it. Makes no cloud calls. |
| `upgrade` | Diff the target chart's `values.schema.json` against the installed one, guide you through breaking changes, and upgrade. |
| `uninstall` | Remove the release and in-cluster databases; optionally delete managed cloud services and the namespace. |
| `version` | Print the version. |

Run any command with `--help` for its full flag list. `--plain` forces plain,
sequential prompts (useful over SSH or in scripts). `--json` makes `preflight`,
`permissions`, and `upgrade` emit machine-readable output.

## The zero-networking contract

The installer creates **no networking** on any cloud. It never makes a VPC, subnet,
subnet group, security group, private endpoint, DNS zone, or peering, and it never
creates a cloud project or account. When you choose managed databases, you name
networking that already exists and the installer places the database into it.

This keeps a boundary that most platform teams want: the team that owns the VPC owns
the network, and the person running the installer does not need permission to change
it. Each per-cloud guide gives the commands to create that networking, or you can
hand it to your infra team with `llamacloudctl terraform` (below).

What you supply per cloud:

- **AWS**: a DB subnet group, a cache subnet group, and security group ID(s) that
  let the cluster reach PostgreSQL on 5432 and Redis on 6379.
- **GCP**: a VPC network with a private-services-access connection, and the name of
  that connection's allocated IP range. Cloud SQL and Memorystore sit on that range.
- **Azure**: a subnet already delegated to
  `Microsoft.DBforPostgreSQL/flexibleServers`, a private DNS zone ending
  `.postgres.database.azure.com`, and a private endpoint fronting the Redis cluster.
  Azure Managed Redis takes no networking input of its own, so it is created with
  public access disabled and stays unreachable until you attach that endpoint.

## Hand cloud provisioning to your infra team

If you do not have the cloud permissions to create the buckets, identity, or managed
databases, emit Terraform and pass it to whoever does:

```sh
# Emit a bundle for AWS managed data services into ./tf
llamacloudctl terraform --cloud aws --managed all --out-dir ./tf -f values.yaml

# They fill in the existing networking, apply, and capture the outputs:
#   cp terraform.tfvars.example terraform.tfvars   # edit in the networking IDs
#   terraform init && terraform apply
#   terraform output -json > outputs.json

# You finish the install against those outputs:
llamacloudctl install -n llamacloud --from-terraform outputs.json
```

The bundle consumes existing networking as variables and creates none. The
`terraform output -json` document carries the database endpoints, credentials,
bucket names, and identity ARN under a fixed set of keys that `--from-terraform`
reads back in. The per-cloud guides show the full round trip.

## Upgrades

```sh
# Upgrade the release in the namespace to a target chart version:
llamacloudctl upgrade --chart-version <version>

# Diff and migrate without running the upgrade:
llamacloudctl upgrade --chart-version <version> --dry-run
```

`upgrade` fetches the target chart, diffs its `values.schema.json` against the one
the release was installed with, and walks you through any breaking changes in the
values you actually set. It re-validates the result before upgrading and saves the
migrated values file. Keep that file.

## Uninstall

```sh
# Remove the in-cluster releases only (prompts for the rest):
llamacloudctl uninstall -n llamacloud

# Full teardown, non-interactive: releases, managed cloud services, and the namespace:
llamacloudctl uninstall -n llamacloud --yes \
  --cloud aws --region us-east-1 --delete-managed all --delete-namespace
```

`--delete-managed` destroys the managed cloud databases and skips final snapshots,
so confirm the names first. On Azure and GCP, name the cloud and its location or
project so the command can find the instances. StatefulSet PersistentVolumeClaims
survive an uninstall by design; the command lists any it leaves behind, with the
`kubectl` command to delete them.

An install that failed or timed out leaves its release in Helm's `failed` or
`pending-install` state. `uninstall` removes those too, so a stuck release cannot
block the next install with "cannot re-use a name that is still in use".
