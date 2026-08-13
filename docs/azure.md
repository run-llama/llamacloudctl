# Deploy on Azure (AKS)

This guide walks a full install on AKS with managed Azure data services: PostgreSQL
Flexible Server, Azure Managed Redis, and Blob Storage. It uses the zero-networking
contract, so you create the networking first and the installer places the databases
into it.

Azure Managed Redis is the one case where a networking step happens *after* the
installer runs. The private endpoint can only be created once the Redis cluster
exists. [Step 6](#6-attach-the-redis-private-endpoint) covers it.

For in-cluster PostgreSQL and Redis instead of managed Azure services, skip the
networking and managed-database steps and pick "install in-cluster" at those prompts.

## Prerequisites

- An AKS cluster, with `kubectl` pointed at it.
- `az`, logged in and set to the right subscription.
- `llamacloudctl` on your `PATH` ([install instructions](../README.md#install-the-cli)).
- A LlamaCloud license JWT and an embeddings-capable LLM key (for example OpenAI).

Set these once:

```sh
export SUB=<subscription-id>
export RG=<resource-group>
export VNET=<vnet-name>
export LOCATION=southcentralus
export NAMESPACE=llamacloud
az account set --subscription "$SUB"
```

## 1. Create the PostgreSQL networking

Flexible Server needs a subnet delegated to it and a private DNS zone. Create both,
and link the zone to your VNet. The installer creates none of this.

```sh
# A /24 subnet delegated to Flexible Server:
az network vnet subnet create -g "$RG" --vnet-name "$VNET" \
  -n llamacloud-pg-subnet --address-prefixes 10.128.136.0/24 \
  --delegations Microsoft.DBforPostgreSQL/flexibleServers

# The private DNS zone Flexible Server uses, linked to the VNet:
az network private-dns zone create -g "$RG" \
  -n privatelink.postgres.database.azure.com
az network private-dns link vnet create -g "$RG" \
  -z privatelink.postgres.database.azure.com \
  -n llamacloud-pg-link --virtual-network "$VNET" --registration-enabled false
```

Capture the resource IDs the installer needs:

```sh
export PG_SUBNET_ID=$(az network vnet subnet show -g "$RG" --vnet-name "$VNET" \
  -n llamacloud-pg-subnet --query id -o tsv)
export PG_DNS_ZONE_ID=$(az network private-dns zone show -g "$RG" \
  -n privatelink.postgres.database.azure.com --query id -o tsv)
```

## 2. Create the object-storage container

Blob Storage is reached through the chart's s3proxy shim, which speaks the S3 API and
authenticates with a storage account key.

```sh
export SA=<storage-account-name>
az storage container create --account-name "$SA" --name llamacloud --auth-mode login
export SA_KEY=$(az storage account keys list --account-name "$SA" -g "$RG" \
  --query '[0].value' -o tsv)
```

## 3. Provide the license

```sh
kubectl create namespace "$NAMESPACE"
kubectl create secret generic llamacloud-license-key -n "$NAMESPACE" \
  --from-literal=LLAMACLOUD_LICENSE_KEY='<your-license-jwt>'
```

## 4. Run the install

You need the intended Redis private endpoint's resource ID now, even though you
create the endpoint later (step 6). Pick a name for it:

```sh
export REDIS_PE_ID="/subscriptions/$SUB/resourceGroups/$RG/providers/Microsoft.Network/privateEndpoints/llamacloud-redis-pe"

llamacloudctl install -n "$NAMESPACE" \
  --cloud azure \
  --region "$LOCATION" \
  --azure-subscription-id "$SUB" \
  --azure-resource-group "$RG" \
  --pg-delegated-subnet-id "$PG_SUBNET_ID" \
  --pg-private-dns-zone-id "$PG_DNS_ZONE_ID" \
  --redis-private-endpoint-id "$REDIS_PE_ID"
```

Answer the wizard prompts:

1. **Chart version**: press Enter for the latest published.
2. **License**: choose "reference existing secret" and enter `llamacloud-license-key`.
3. **Authentication**: basic or OIDC. For basic auth, give an email domain you
   control that actually accepts mail: the first user signs up with an address at
   that domain and registration verifies the domain is deliverable, so a placeholder
   like `example.com` is rejected. Sign-up passwords must be at least 16 characters.
4. **LLM providers**: select at least one embeddings-capable provider.
5. **Object storage**: choose Azure. Enter the endpoint
   `https://<storage-account>.blob.core.windows.net`, the storage account name as the
   identity, the storage account key as the credential, and `llamacloud` as the
   bucket.
6. **Data services**: choose "provision managed" for PostgreSQL and Redis. The
   subscription, resource group, and networking prompts are prefilled from your flags.
7. **Temporal**: install in-cluster.

The installer provisions Flexible Server into your delegated subnet and Managed Redis
with public access disabled, writes their credentials to Kubernetes Secrets, and
starts installing the chart. It sets the PostgreSQL server parameters the platform
needs (the `azure.extensions` allow-list and a connection-count floor) automatically.

The rollout will not finish until Redis is reachable, which needs step 6. Leave the
install running and open a second terminal.

## 5. Confirm Redis is up

```sh
az redis enterprise database show --cluster-name llamacloud-llamacloud-redis \
  -g "$RG" --database-name default --query provisioningState -o tsv
```

Wait for `Succeeded`. Managed Redis creation takes 15-20 minutes.

## 6. Attach the Redis private endpoint

Managed Redis has public access disabled and accepts no networking input of its own,
so it is unreachable until you attach a private endpoint. Create it at the resource ID
you passed in step 4, then wire up the DNS zone so the cluster hostname resolves to
it.

```sh
CLUSTER_ID=$(az resource show -g "$RG" -n llamacloud-llamacloud-redis \
  --resource-type Microsoft.Cache/redisEnterprise --query id -o tsv)

az network private-endpoint create -g "$RG" -n llamacloud-redis-pe \
  --vnet-name "$VNET" --subnet <a-non-delegated-subnet> \
  --private-connection-resource-id "$CLUSTER_ID" \
  --group-id redisEnterprise --connection-name redis

# The hostname is <name>.<region>.redis.azure.net, so the zone is privatelink.redis.azure.net:
az network private-dns zone create -g "$RG" -n privatelink.redis.azure.net
az network private-dns link vnet create -g "$RG" -z privatelink.redis.azure.net \
  -n llamacloud-redis-link --virtual-network "$VNET" --registration-enabled false
az network private-endpoint dns-zone-group create -g "$RG" \
  --endpoint-name llamacloud-redis-pe -n redis \
  --private-dns-zone privatelink.redis.azure.net --zone-name redis
```

Once DNS resolves, the Redis-dependent pods connect and the rollout finishes at
`status=deployed`.

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
llamacloudctl terraform --cloud azure --managed all --out-dir ./tf -f values.yaml
```

They edit `terraform.tfvars` with the existing subscription, resource group, and
networking IDs, `terraform apply`, and send back `terraform output -json > outputs.json`.
You finish with `--from-terraform outputs.json`. The bundle creates no networking.

## Teardown

```sh
# Uninstall the release, delete the managed databases, and remove the namespace:
llamacloudctl uninstall -n "$NAMESPACE" --yes \
  --cloud azure --region "$LOCATION" \
  --azure-subscription-id "$SUB" --azure-resource-group "$RG" \
  --delete-managed all --delete-namespace

# Then the networking you created:
az network private-endpoint delete -g "$RG" -n llamacloud-redis-pe
az network private-dns zone delete -g "$RG" -n privatelink.redis.azure.net --yes
az network vnet subnet delete -g "$RG" --vnet-name "$VNET" -n llamacloud-pg-subnet
az network private-dns zone delete -g "$RG" \
  -n privatelink.postgres.database.azure.com --yes
az storage container delete --account-name "$SA" --name llamacloud --auth-mode login
```

`--delete-managed` skips final snapshots. Rotate the storage account key afterward if
you passed it through the wizard on a shared terminal.
