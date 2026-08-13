package infra

import "fmt"

// This file holds the Azure and GCP operator-permission catalogs. Unlike AWS —
// which has IAM SimulatePrincipalPolicy for a live per-action check — Azure and
// GCP permission verification is surfaced as the STATIC required-roles catalog
// (permissions.go CheckLive returns the static path for non-AWS clouds). The
// Actions field carries provider role names / permission ids; Resources carries
// the scope those roles are granted at.

// azureSteps is the Azure operator-permission catalog: the Azure RBAC roles (and
// the AKS access) the person running the install — or their infra team applying
// the emitted Terraform — needs. Resources are the scopes each role is granted at.
func azureSteps(p Plan) []Step {
	steps := []Step{
		{
			ID:      "create-blob-containers",
			Name:    "Create Blob storage containers",
			Detail:  fmt.Sprintf("create %d object-storage container(s) in location %s", len(p.Buckets), p.Region),
			Actions: []string{"Microsoft.Storage/storageAccounts/write", "Microsoft.Storage/storageAccounts/blobServices/containers/write"},
			Resources: []string{
				"the target resource group (Contributor, or Storage Account Contributor)",
			},
			Note: "Runtime blob access for the platform pods is granted to a user-assigned managed identity via `Storage Blob Data Contributor` — emitted by `llamacloudctl terraform --cloud azure`, not performed by the operator.",
		},
		{
			ID:      "provision-workload-identity",
			Name:    "Create the managed identity + federated credential yourself",
			Detail:  "only needed if you are NOT handing the emitted Terraform to your infra team",
			Actions: []string{"Microsoft.ManagedIdentity/userAssignedIdentities/write", "Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials/write", "Microsoft.Authorization/roleAssignments/write"},
			Resources: []string{
				"the target resource group (Managed Identity Contributor + User Access Administrator / Owner)",
			},
			Optional: true,
			Note:     "Skip this if your infra team applies `llamacloudctl terraform --cloud azure` output — that creates the identity, role assignment, and federated credential for you.",
		},
		{
			ID:        "access-aks-cluster",
			Name:      "Access the target AKS cluster",
			Detail:    "obtain a kubeconfig for the cluster (az aks get-credentials)",
			Actions:   []string{"Microsoft.ContainerService/managedClusters/listClusterUserCredential/action"},
			Resources: []string{"the AKS cluster (Azure Kubernetes Service Cluster User Role)"},
			Optional:  true,
			Note:      "Installing the chart also requires cluster-admin Kubernetes RBAC, granted in-cluster (AKS RBAC / Azure AD), not via Azure RBAC.",
		},
	}

	// When the install will PROVISION managed Azure data services, add one step
	// per managed kind, plus the resource-provider prerequisite. There is no
	// networking step: the provisioners create no VNet, subnet, delegation,
	// private DNS zone or private endpoint — they only place the resource in
	// networking the operator already owns and named — and they never create a
	// subscription or resource group.
	if len(managedKinds(p)) > 0 {
		steps = append(steps, Step{
			ID:      "register-resource-providers",
			Name:    "Register the data-service resource providers",
			Detail:  "Microsoft.DBforPostgreSQL and Microsoft.Cache must be REGISTERED on the target subscription before either resource can be created",
			Actions: []string{"*/register/action"},
			Resources: []string{
				"the target subscription (Contributor, or any role granting */register/action)",
			},
			Optional: true,
			Note:     "Most subscriptions already have these registered. Check with `az provider show -n Microsoft.DBforPostgreSQL --query registrationState` (and Microsoft.Cache); register with `az provider register -n <namespace>`.",
		})
	}
	steps = append(steps, managedStepsFor(azureManagedCatalog, p.Managed)...)

	// With --teardown the check also covers what `uninstall --delete-managed
	// --cloud azure` needs, with the same declared-vs-inferred provenance the AWS
	// catalog applies.
	if p.Teardown {
		kinds := knownManagedKinds
		if p.ManagedExplicit {
			kinds = p.Managed
		}
		steps = append(steps, teardownStepsFor(azureManagedCatalog, kinds, p.ManagedExplicit)...)
	}
	return steps
}

// azureManagedCatalog is the Azure arm of the managed permission catalog.
//
// The permission sets mirror the minimal Azure surfaces declared in
// internal/provision/azure.go — nothing more. Note redisEnterprise/databases/
// listKeys/action in particular: it is what makes the frozen redis_auth_token
// output obtainable at all, and it is precisely the permission the read-only
// role withholds.
//
// The teardown actions KEEP IN SYNC with the Destroy methods in
// internal/provision/deprovision.go: both delete directly. Neither Azure data
// service has a deletion-protection flag to clear first — Azure PostgreSQL
// Flexible Server has none at all — which is why there is no update/write action
// here to match Cloud SQL's instances.update.
var azureManagedCatalog = managedCatalog{
	provisionActions: map[string][]string{
		"postgres": {
			"Microsoft.DBforPostgreSQL/flexibleServers/read",
			"Microsoft.DBforPostgreSQL/flexibleServers/write",
			"Microsoft.DBforPostgreSQL/flexibleServers/databases/write",
			// The azure.extensions allow-list and max_connections. Azure refuses
			// CREATE EXTENSION for anything not named in the former, so without these
			// two the install still creates a server — and then dies on temporal's
			// btree_gin schema hook; and the latter is capped from the SKU at creation
			// and has to be raised explicitly or the backend exhausts it.
			"Microsoft.DBforPostgreSQL/flexibleServers/configurations/read",
			"Microsoft.DBforPostgreSQL/flexibleServers/configurations/write",
			// max_connections is a STATIC parameter: the create path restarts the
			// server it just created to activate the new value.
			"Microsoft.DBforPostgreSQL/flexibleServers/restart/action",
		},
		"redis": {
			"Microsoft.Cache/redisEnterprise/read",
			"Microsoft.Cache/redisEnterprise/write",
			"Microsoft.Cache/redisEnterprise/databases/read",
			"Microsoft.Cache/redisEnterprise/databases/write",
			"Microsoft.Cache/redisEnterprise/databases/listKeys/action",
			"Microsoft.Cache/locations/operationsStatus/read",
			"Microsoft.Cache/locations/checknameavailability/action",
		},
	},
	teardownActions: map[string][]string{
		"postgres": {"Microsoft.DBforPostgreSQL/flexibleServers/delete"},
		"redis":    {"Microsoft.Cache/redisEnterprise/delete"},
	},
	label: map[string]string{
		"postgres": "Azure Database for PostgreSQL Flexible Server",
		"redis":    "Azure Managed Redis",
	},
	resources: map[string][]string{
		// Stated honestly rather than inventing a narrower role: Azure ships no
		// PostgreSQL-specific built-in provisioning role, so Contributor on the
		// resource group is the real answer.
		"postgres": {"the target resource group (Contributor — there is no PostgreSQL-specific built-in provisioning role)"},
		// "Azure Managed Redis Reader" (f287ba2f-f923-4464-a5bd-721c3951d32d) is
		// INSUFFICIENT: it explicitly cannot retrieve access keys, which is the one
		// thing the credential flow depends on. The classic "Redis Cache
		// Contributor" role is also wrong — it covers Microsoft.Cache/Redis, NOT
		// Microsoft.Cache/redisEnterprise.
		"redis": {"the target resource group (Azure Managed Redis Contributor, 3015e5ed-6856-4ab3-b2f0-b8492aa30ca6)"},
	},
	defaultResources: []string{"the target resource group"},
	provisionNote:    "Also writes a Kubernetes Secret with the retrieved credentials (in-cluster RBAC, not Azure RBAC). Prefer handing `llamacloudctl terraform --cloud azure --managed postgres,redis` output to your infra team if you lack these Azure permissions.",
	teardownNote:     "Teardown removes only the Flexible Server / Redis cluster and its credential Secret (Secret deletion is in-cluster RBAC, not Azure RBAC). Deleting the Redis cluster removes its database with it.",
}

// gcpSteps is the GCP operator-permission catalog: the IAM permissions (and the
// GKE access) the person running the install — or their infra team applying the
// emitted Terraform — needs. Resources are the scope each permission applies at.
func gcpSteps(p Plan) []Step {
	steps := []Step{
		{
			ID:        "create-gcs-buckets",
			Name:      "Create GCS buckets",
			Detail:    fmt.Sprintf("create %d object-storage bucket(s) in region %s", len(p.Buckets), p.Region),
			Actions:   []string{"storage.buckets.create", "storage.buckets.get"},
			Resources: []string{"the target project (roles/storage.admin)"},
			Note:      "Runtime object access for the platform pods is granted to a Google service account via roles/storage.objectAdmin — emitted by `llamacloudctl terraform --cloud gcp`, not performed by the operator.",
		},
		{
			ID:      "provision-workload-identity",
			Name:    "Create the service account + Workload Identity binding yourself",
			Detail:  "only needed if you are NOT handing the emitted Terraform to your infra team",
			Actions: []string{"iam.serviceAccounts.create", "iam.serviceAccounts.setIamPolicy", "resourcemanager.projects.setIamPolicy"},
			Resources: []string{
				"the target project (roles/iam.serviceAccountAdmin + roles/resourcemanager.projectIamAdmin)",
			},
			Optional: true,
			Note:     "Skip this if your infra team applies `llamacloudctl terraform --cloud gcp` output — that creates the service account, bucket IAM, and Workload Identity binding for you.",
		},
		{
			ID:        "access-gke-cluster",
			Name:      "Access the target GKE cluster",
			Detail:    "obtain a kubeconfig for the cluster (gcloud container clusters get-credentials)",
			Actions:   []string{"container.clusters.get"},
			Resources: []string{"the GKE cluster (roles/container.clusterViewer)"},
			Optional:  true,
			Note:      "Installing the chart also requires cluster-admin Kubernetes RBAC, granted in-cluster (GKE RBAC), not via GCP IAM.",
		},
	}

	// When the install will PROVISION managed GCP data services, add one step per
	// managed kind. There is no networking step: the provisioners create no VPC, no
	// subnet, no allocated range and no peering — they only place the instance in
	// networking the operator already owns and named — and they never create a
	// project.
	steps = append(steps, managedStepsFor(gcpManagedCatalog, p.Managed)...)

	// With --teardown the check also covers what `uninstall --delete-managed
	// --cloud gcp` needs, with the same declared-vs-inferred provenance the AWS
	// catalog applies.
	if p.Teardown {
		kinds := knownManagedKinds
		if p.ManagedExplicit {
			kinds = p.Managed
		}
		steps = append(steps, teardownStepsFor(gcpManagedCatalog, kinds, p.ManagedExplicit)...)
	}
	return steps
}

// gcpManagedCatalog is the GCP arm of the managed permission catalog.
//
// The permission sets mirror the minimal GCP surfaces declared in
// internal/provision/gcp.go — nothing more. In particular there is NO
// cloudsql.users.* permission: the design sets the built-in postgres role's
// password through the instance's RootPassword field (SDK) / root_password
// attribute (Terraform) and never touches the Users API, so requesting a users
// permission would over-request access no code path uses.
//
// The teardown actions KEEP IN SYNC with the Destroy methods in
// internal/provision/deprovision.go: Cloud SQL clears deletion protection
// (instances.update) then deletes; Memorystore deletes directly, having no
// protection flag to clear.
var gcpManagedCatalog = managedCatalog{
	provisionActions: map[string][]string{
		"postgres": {
			"cloudsql.instances.create",
			"cloudsql.instances.get",
			"cloudsql.instances.list",
			"cloudsql.databases.create",
		},
		"redis": {
			"redis.instances.create",
			"redis.instances.get",
			"redis.instances.list",
			"redis.instances.getAuthString",
		},
	},
	teardownActions: map[string][]string{
		"postgres": {
			"cloudsql.instances.update",
			"cloudsql.instances.delete",
		},
		"redis": {
			"redis.instances.delete",
		},
	},
	label: map[string]string{
		"postgres": "Cloud SQL (PostgreSQL)",
		"redis":    "Memorystore (Redis)",
	},
	resources: map[string][]string{
		"postgres": {"the target project (roles/cloudsql.admin)"},
		"redis":    {"the target project (roles/redis.admin)"},
	},
	defaultResources: []string{"the target project"},
	provisionNote:    "Also writes a Kubernetes Secret with the retrieved credentials (in-cluster RBAC, not IAM). Prefer handing `llamacloudctl terraform --cloud gcp --managed postgres,redis` output to your infra team if you lack these GCP permissions.",
	teardownNote:     "Teardown removes only the Cloud SQL / Memorystore instance and its credential Secret (Secret deletion is in-cluster RBAC, not IAM).",
}
