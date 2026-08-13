# Deploy on AWS (EKS)

This guide walks a full install on EKS with managed AWS data services: RDS for
PostgreSQL, ElastiCache for Redis, and S3 for object storage. It uses the
zero-networking contract, so you create the networking first and the installer
places the databases into it.

For in-cluster PostgreSQL and Redis instead of managed AWS services, skip the
[networking](#1-create-the-networking-managed-databases-only) and
[managed database](#4-run-the-install) steps and pick "install in-cluster" at those
prompts. You still need a StorageClass (step 3).

## Prerequisites

- An EKS cluster, with `kubectl` pointed at it and `aws` authenticated to the same
  account.
- `llamacloudctl` on your `PATH` ([install instructions](../README.md#install-the-cli)).
- A LlamaCloud license JWT and an embeddings-capable LLM key (for example OpenAI).
- Permission to create RDS, ElastiCache, S3, and (for object-storage IRSA) an IAM
  role. Check with `llamacloudctl permissions --managed all`; if you lack any, use
  the [Terraform hand-off](#alternative-hand-it-to-your-infra-team).

Set these once so the commands below are copy-paste:

```sh
export AWS_REGION=us-east-1
export CLUSTER=my-eks-cluster
export NAMESPACE=llamacloud
export VPC_ID=$(aws eks describe-cluster --name "$CLUSTER" \
  --query 'cluster.resourcesVpcConfig.vpcId' --output text)
```

## 1. Create the networking (managed databases only)

The installer never creates networking. Create a DB subnet group, a cache subnet
group, and a security group that lets the cluster reach the databases. Use two
private subnets in different Availability Zones.

```sh
# Two private subnets in the cluster VPC, in different AZs:
SUBNETS=$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$VPC_ID" \
  --query 'Subnets[?MapPublicIpOnLaunch==`false`].SubnetId' --output text)

aws rds create-db-subnet-group \
  --db-subnet-group-name llamacloud-db \
  --db-subnet-group-description "LlamaCloud managed PostgreSQL" \
  --subnet-ids $SUBNETS

aws elasticache create-cache-subnet-group \
  --cache-subnet-group-name llamacloud-cache \
  --cache-subnet-group-description "LlamaCloud managed Redis" \
  --subnet-ids $SUBNETS

# A security group the cluster can reach PostgreSQL (5432) and Redis (6379) through.
# Allow the VPC CIDR, or scope it to your node security group.
VPC_CIDR=$(aws ec2 describe-vpcs --vpc-ids "$VPC_ID" \
  --query 'Vpcs[0].CidrBlock' --output text)
SG_ID=$(aws ec2 create-security-group --group-name llamacloud-db-sg \
  --description "LlamaCloud managed databases" --vpc-id "$VPC_ID" \
  --query 'GroupId' --output text)
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
  --protocol tcp --port 5432 --cidr "$VPC_CIDR"
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
  --protocol tcp --port 6379 --cidr "$VPC_CIDR"
```

## 2. Provide the license

Create a Secret that holds the license, and reference it in the wizard. This keeps
the JWT out of your shell history.

```sh
kubectl create namespace "$NAMESPACE"
kubectl create secret generic llamacloud-license-key -n "$NAMESPACE" \
  --from-literal=LLAMACLOUD_LICENSE_KEY='<your-license-jwt>'
```

## 3. Check readiness and permissions

```sh
llamacloudctl preflight -n "$NAMESPACE"
llamacloudctl permissions --managed all --region "$AWS_REGION"
```

`preflight` is read-only. If it reports no default StorageClass (common on EKS), note
your class name (`gp2` on most clusters) and pass it as `--storage-class` below.
`permissions` runs a live IAM simulation; if a step is DENIED it prints a
least-privilege policy to hand your administrator.

## 4. Run the install

```sh
llamacloudctl install -n "$NAMESPACE" \
  --cluster "$CLUSTER" \
  --region "$AWS_REGION" \
  --storage-class gp2 \
  --db-subnet-group-name llamacloud-db \
  --db-security-group-ids "$SG_ID" \
  --cache-subnet-group-name llamacloud-cache \
  --cache-security-group-ids "$SG_ID"
```

The flags prefill the wizard's networking prompts. Answer the rest as they come:

1. **Chart version**: press Enter for the latest published.
2. **License**: choose "reference existing secret" and enter `llamacloud-license-key`.
3. **Authentication**: basic (email + password) or OIDC. For basic auth, give an
   email domain you control that actually accepts mail: the first user signs up with
   an address at that domain and registration verifies the domain is deliverable, so
   a placeholder like `example.com` is rejected. Sign-up passwords must be at least
   16 characters.
4. **LLM providers**: select at least one embeddings-capable provider and enter its
   key. For Bedrock, pick "IAM role" if the pods use IRSA or the node role; you need
   no static keys, just grant that role `bedrock:InvokeModel`.
5. **Object storage**: choose AWS, then "create buckets for me" (needs
   `s3:CreateBucket`) or "use existing buckets". Authenticate with IRSA (recommended)
   or a static access key.
6. **Data services**: choose "provision managed" for PostgreSQL and Redis. The
   subnet group and security group prompts are prefilled from your flags.
7. **Temporal**: install in-cluster (the default).

The installer creates the S3 bucket (if requested), provisions RDS and ElastiCache
into your subnet groups and security group, writes their generated credentials to
Kubernetes Secrets, installs the chart, and streams the rollout to
`status=deployed`. Managed database creation takes 5-15 minutes; the installer
prints progress while it waits.

The same-account guard refuses to create S3 buckets in a different AWS account than
the cluster. If that is intentional, pass `--allow-cross-account`.

## 5. Verify

```sh
kubectl get pods -n "$NAMESPACE"          # all Running/Ready
llamacloudctl preflight -n "$NAMESPACE"    # re-check against the live values
```

If the rollout timed out, the streamed output already named the failing pods and
reasons. Start there.

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

## Reproduce the install non-interactively

Every wizard run saves its answers to `<namespace>-values.yaml`, and prints the exact
replay command. Because the create-buckets and in-cluster choices are not stored in
the values file, the replay carries them as flags:

```sh
llamacloudctl install -n "$NAMESPACE" -f llamacloud-values.yaml --yes \
  --region "$AWS_REGION" --create-buckets
```

## Alternative: hand it to your infra team

If you lack the cloud permissions, emit Terraform and pass it on:

```sh
llamacloudctl terraform --cloud aws --managed all --out-dir ./tf -f values.yaml
```

They edit `terraform.tfvars` with the existing subnet groups and security group IDs,
`terraform apply`, and send back `terraform output -json > outputs.json`. You finish:

```sh
llamacloudctl install -n "$NAMESPACE" --from-terraform outputs.json --yes
```

The bundle creates no networking. It reads the subnet groups and security groups you
name as variables.

## Teardown

```sh
# Uninstall the release, delete the managed databases, and remove the namespace:
llamacloudctl uninstall -n "$NAMESPACE" --yes \
  --cloud aws --region "$AWS_REGION" --delete-managed all --delete-namespace

# Then delete the networking you created:
aws rds delete-db-subnet-group --db-subnet-group-name llamacloud-db
aws elasticache delete-cache-subnet-group --cache-subnet-group-name llamacloud-cache
aws ec2 delete-security-group --group-id "$SG_ID"
```

`--delete-managed` skips final snapshots. If you created S3 buckets, empty and delete
them separately. StatefulSet PersistentVolumeClaims survive the uninstall; the
command lists any it leaves behind with the `kubectl` command to remove them.
