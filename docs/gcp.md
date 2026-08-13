# Deploy on GCP (GKE)

This guide walks a full install on GKE with managed GCP data services: Cloud SQL for
PostgreSQL, Memorystore for Redis, and GCS for object storage. It uses the
zero-networking contract, so you create the VPC and private-services-access
connection first and the installer places the databases onto that range.

For in-cluster PostgreSQL and Redis instead of managed GCP services, skip the
[networking](#2-create-the-networking-managed-databases-only) and
[managed database](#6-run-the-install) steps and pick "install in-cluster" at those
prompts.

## Prerequisites

- A GKE cluster (Autopilot or Standard), with `kubectl` pointed at it.
- `gcloud`, and the GKE auth plugin. GKE contexts need `gke-gcloud-auth-plugin`; if
  `kubectl` fails with `executable gke-gcloud-auth-plugin not found`, install it with
  `gcloud components install gke-gcloud-auth-plugin` and make sure the SDK `bin`
  directory is on your `PATH`.
- `llamacloudctl` on your `PATH` ([install instructions](../README.md#install-the-cli)).
- A LlamaCloud license JWT and an embeddings-capable LLM key (for example OpenAI).

Set these once:

```sh
export PROJECT=my-gcp-project
export REGION=us-central1
export NAMESPACE=llamacloud
gcloud config set project "$PROJECT"
```

## 1. Enable APIs and application-default credentials

The managed provisioners use Application Default Credentials, which are separate from
your `gcloud` user login. Mint them, or Cloud SQL and Memorystore creation fails with
`could not find default credentials`.

```sh
gcloud services enable container.googleapis.com sqladmin.googleapis.com \
  redis.googleapis.com servicenetworking.googleapis.com compute.googleapis.com

gcloud auth application-default login
gcloud auth application-default set-quota-project "$PROJECT"
```

## 2. Create the networking (managed databases only)

Cloud SQL and Memorystore reach the cluster over a private-services-access (PSA)
connection. Create a VPC, a subnet, an allocated IP range for PSA, and the peering.
The installer creates none of this.

```sh
gcloud compute networks create llamacloud-vpc --subnet-mode=custom

gcloud compute networks subnets create llamacloud-subnet \
  --network=llamacloud-vpc --region="$REGION" --range=10.10.0.0/20

gcloud compute addresses create llamacloud-psa \
  --global --purpose=VPC_PEERING --prefix-length=16 --network=llamacloud-vpc

gcloud services vpc-peerings connect \
  --service=servicenetworking.googleapis.com \
  --ranges=llamacloud-psa --network=llamacloud-vpc
```

If your GKE cluster is not already on `llamacloud-vpc`, create one that is:

```sh
gcloud container clusters create-auto llamacloud-gke \
  --region "$REGION" --network llamacloud-vpc --subnetwork llamacloud-subnet
gcloud container clusters get-credentials llamacloud-gke --region "$REGION"
```

## 3. Create the object-storage bucket and an HMAC key

GCS is reached through the chart's s3proxy shim, which speaks the S3 API and
authenticates with an HMAC key. Create a bucket, a service account with object
access, and an HMAC key for it.

```sh
gsutil mb -l "$REGION" "gs://llamacloud-$PROJECT"

gcloud iam service-accounts create llamacloud-s3proxy \
  --display-name "LlamaCloud s3proxy"
gsutil iam ch \
  "serviceAccount:llamacloud-s3proxy@$PROJECT.iam.gserviceaccount.com:roles/storage.objectAdmin" \
  "gs://llamacloud-$PROJECT"

# Capture BOTH the access ID and the secret; the secret is shown only once.
gcloud storage hmac create "llamacloud-s3proxy@$PROJECT.iam.gserviceaccount.com"
```

## 4. Provide the license

```sh
kubectl create namespace "$NAMESPACE"
kubectl create secret generic llamacloud-license-key -n "$NAMESPACE" \
  --from-literal=LLAMACLOUD_LICENSE_KEY='<your-license-jwt>'
```

## 5. Check readiness

```sh
llamacloudctl preflight -n "$NAMESPACE"
```

`preflight` is read-only. On GKE Autopilot it reports the node-capacity check as
advisory rather than a failure, because Autopilot provisions nodes on demand as the
chart's pods schedule.

## 6. Run the install

```sh
llamacloudctl install -n "$NAMESPACE" \
  --cloud gcp \
  --region "$REGION" \
  --gcp-project "$PROJECT" \
  --gcp-vpc-network "projects/$PROJECT/global/networks/llamacloud-vpc" \
  --gcp-allocated-ip-range llamacloud-psa
```

The flags prefill the wizard's networking prompts. Answer the rest:

1. **Chart version**: press Enter for the latest published.
2. **License**: choose "reference existing secret" and enter `llamacloud-license-key`.
3. **Authentication**: basic or OIDC. For basic auth, give an email domain you
   control that actually accepts mail: the first user signs up with an address at
   that domain and registration verifies the domain is deliverable, so a placeholder
   like `example.com` is rejected. Sign-up passwords must be at least 16 characters.
4. **LLM providers**: select at least one embeddings-capable provider.
5. **Object storage**: choose GCP. Enter the endpoint `https://storage.googleapis.com`,
   the HMAC access ID as the identity, the HMAC secret as the credential, and your
   bucket name (`llamacloud-$PROJECT`).
6. **Data services**: choose "provision managed" for PostgreSQL and Redis. The VPC
   network and allocated-range prompts are prefilled from your flags.
7. **Temporal**: install in-cluster.

The installer provisions Cloud SQL and Memorystore onto your PSA range, writes their
credentials to Kubernetes Secrets, installs the chart, and streams the rollout to
`status=deployed`. Cloud SQL creation takes 10-15 minutes; the installer prints
progress while it waits.

## 7. Verify

```sh
kubectl get pods -n "$NAMESPACE"
llamacloudctl preflight -n "$NAMESPACE"
```

## Parse a document end to end

`kubectl get pods` and `preflight` prove the pods started. They do not prove object
storage, the parse pipeline, and Temporal actually work together. Parse a document to
confirm that, using the bundled smoke test.

```sh
# 1. Forward the API service to your machine:
kubectl port-forward -n "$NAMESPACE" svc/llamacloud 8000:80

# 2. Create a project API key in the LlamaCloud UI (Settings > API Keys), then:
BASE_URL=http://localhost:8000 LLAMA_CLOUD_API_KEY=<your-key> \
  scripts/parse-smoke.sh
```

It uploads a one-page PDF, polls the parse job to completion, and prints the markdown
result. A `PASS` line means object storage, parsing, Temporal, and the database all
work end to end. This is the check that catches a storage backend that is misconfigured
but health-green.

## Alternative: hand it to your infra team

```sh
llamacloudctl terraform --cloud gcp --managed all --out-dir ./tf -f values.yaml
```

They edit `terraform.tfvars` with the existing project, VPC network, and allocated
range, `terraform apply`, and send back `terraform output -json > outputs.json`. You
finish with `--from-terraform outputs.json`. The bundle creates no networking and no
project.

## Teardown

```sh
# Uninstall the release, delete the managed databases, and remove the namespace:
llamacloudctl uninstall -n "$NAMESPACE" --yes \
  --cloud gcp --region "$REGION" --gcp-project "$PROJECT" \
  --delete-managed all --delete-namespace

# Then delete the networking, in reverse order:
gcloud services vpc-peerings delete \
  --service=servicenetworking.googleapis.com --network=llamacloud-vpc
gcloud compute addresses delete llamacloud-psa --global
gcloud compute networks subnets delete llamacloud-subnet --region "$REGION"
gcloud compute networks delete llamacloud-vpc

# And the storage fixtures:
gsutil rb "gs://llamacloud-$PROJECT"
gcloud iam service-accounts delete "llamacloud-s3proxy@$PROJECT.iam.gserviceaccount.com"
```

Cloud SQL instances carry deletion protection. `--delete-managed` clears it before
deleting; if you tear the instance down by hand, run
`gcloud sql instances patch <name> --no-deletion-protection` first.
