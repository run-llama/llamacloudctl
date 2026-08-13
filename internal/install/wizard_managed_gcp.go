package install

import (
	"fmt"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/tui"
)

// This file holds the PER-CLOUD halves of the wizard's managed branch: the notes
// that describe what managed provisioning does on the target cloud, and the
// networking collectors that gather the operator-supplied inputs each cloud needs.
//
// The AWS wording is reproduced BYTE-IDENTICALLY, so an operator on the shipped
// path sees exactly what they saw before. A new cloud adds one arm to each
// switch — the shape the Azure managed path extends.

// managedIntroBullet renders the "provision managed" bullet in the data-services
// intro note, in the target cloud's own vocabulary. It returns "" for a cloud with
// no managed provisioners, so the bullet is omitted exactly where the option is.
func managedIntroBullet(c cloud.Cloud) string {
	if !handoffAvailable(c) {
		return ""
	}
	if c == cloud.GCP {
		return "  • provision managed .... the installer creates a GCP managed service\n" +
			"                           (postgres→Cloud SQL, redis→Memorystore). Managed needs\n" +
			"                           an EXISTING project, VPC network + allocated IP range\n" +
			"                           you name, and GCP provisioning permissions. The\n" +
			"                           installer changes no networking.\n"
	}
	if c == cloud.Azure {
		return "  • provision managed .... the installer creates an Azure managed service\n" +
			"                           (postgres→Database for PostgreSQL Flexible Server,\n" +
			"                           redis→Azure Managed Redis). Managed needs an EXISTING\n" +
			"                           resource group, delegated subnet + private DNS zone,\n" +
			"                           and a private endpoint you name, plus Azure\n" +
			"                           provisioning permissions. The installer changes no\n" +
			"                           networking.\n"
	}
	return "  • provision managed .... the installer creates an AWS managed service\n" +
		"                           (postgres→RDS, redis→ElastiCache). Managed needs an\n" +
		"                           EXISTING subnet group + security group(s) you name,\n" +
		"                           and AWS provisioning permissions. The installer\n" +
		"                           changes no networking.\n"
}

// managedServiceNote renders the per-service note shown just before the networking
// prompts.
//
// Two clouds carry an extra TLS-posture paragraph, and their polarity is
// DIFFERENT — say which, because "there is a TLS caveat" is not the same warning
// twice:
//   - GCP redis (Memorystore): the traffic is AUTH-protected over private service
//     access but NOT TLS-encrypted at all, because the chart cannot be given the
//     instance's CA to verify against.
//   - Azure postgres (Flexible Server): the connection IS encrypted; only the
//     server CERTIFICATE goes unverified.
//
// An operator choosing either must learn it here, at the prompt, not from a code
// comment.
func managedServiceNote(c cloud.Cloud, kind string) string {
	if c == cloud.Azure {
		note := fmt.Sprintf("Managed provisioning for %s (Azure)\n"+
			"\n"+
			"The installer will create-or-adopt an Azure managed resource in your\n"+
			"resource group when you approve the plan — this can take several minutes\n"+
			"and incurs Azure cost. It creates the resource in networking YOU name, and\n"+
			"CHANGES NO NETWORKING itself: no VNet, no subnet, no subnet delegation, no\n"+
			"private DNS zone, no private endpoint. It never creates a subscription or a\n"+
			"resource group.\n"+
			"So it requires:\n"+
			"  • an EXISTING subscription and resource group;\n"+
			"  • for postgres, an EXISTING subnet that already carries the\n"+
			"    Microsoft.DBforPostgreSQL/flexibleServers delegation, and an EXISTING\n"+
			"    private DNS zone ending .postgres.database.azure.com;\n"+
			"  • for redis, an EXISTING private endpoint (GroupId \"redisEnterprise\",\n"+
			"    zone privatelink.redis.azure.net). Azure Managed Redis accepts no\n"+
			"    networking input of its own, so the cluster is created with public\n"+
			"    access DISABLED and is UNREACHABLE until your private endpoint is\n"+
			"    attached — the installer neither creates nor verifies it;\n"+
			"  • if you do not know these, ask whoever owns your VNet;\n"+
			"  • Azure permissions to create the service. If your Azure access is\n"+
			"    limited, run `llamacloudctl permissions` to see exactly what to request,\n"+
			"    or hand `llamacloudctl terraform` to whoever owns the subscription so\n"+
			"    THEY provision it and you pick \"existing endpoint\" instead.", kind)
		if kind == "postgres" {
			note += "\n" +
				"\n" +
				"NOTE on Azure PostgreSQL security: the connection IS encrypted — Flexible\n" +
				"Server requires TLS — but the server CERTIFICATE is NOT VERIFIED by the\n" +
				"platform, and that cannot be changed from values. The llamacloud chart has\n" +
				"no sslmode key at all, and the platform's asyncpg client defaults to\n" +
				"ssl='prefer'. So this is weaker than full verification but, unlike the GCP\n" +
				"Memorystore case, it is NOT plaintext: the traffic is encrypted in transit,\n" +
				"and the server is additionally reachable only from inside your VNet through\n" +
				"the delegated subnet and private DNS zone."
		}
		return note
	}
	if c == cloud.GCP {
		note := fmt.Sprintf("Managed provisioning for %s (GCP)\n"+
			"\n"+
			"The installer will create-or-adopt a GCP managed resource in your project\n"+
			"when you approve the plan — this can take several minutes and incurs GCP\n"+
			"cost. It creates the resource in networking YOU name, and CHANGES NO\n"+
			"NETWORKING itself: no VPC network, no subnet, no allocated IP range, no\n"+
			"peering. It never creates a project.\n"+
			"So it requires:\n"+
			"  • an EXISTING project, an EXISTING VPC network, and the NAME of an\n"+
			"    EXISTING allocated IP range whose private services access connection is\n"+
			"    already established — if you do not know these, ask whoever owns your\n"+
			"    VPC;\n"+
			"  • GCP permissions to create the service. If your GCP access is limited,\n"+
			"    run `llamacloudctl permissions` to see exactly what to request, or hand\n"+
			"    `llamacloudctl terraform` to whoever owns the project so THEY provision\n"+
			"    it and you pick \"existing endpoint\" instead.", kind)
		if kind == "redis" {
			note += "\n" +
				"\n" +
				"NOTE on Memorystore security: the instance is created with AUTH enabled and\n" +
				"is reachable only over private service access inside your VPC, but transit\n" +
				"encryption is DISABLED and the connection scheme is redis://, not rediss://.\n" +
				"Memorystore's TLS mode presents a per-instance Google-managed CA, and the\n" +
				"llamacloud chart has no field to carry that CA — so a rediss:// URL could not\n" +
				"be verified by the client. An unverifiable TLS connection would be worse than\n" +
				"an honest plaintext one, so the traffic is protected by the AUTH string and\n" +
				"network isolation instead."
		}
		return note
	}
	return fmt.Sprintf("Managed provisioning for %s (AWS)\n"+
		"\n"+
		"The installer will create-or-adopt an AWS managed resource in your account\n"+
		"when you approve the plan — this can take several minutes and incurs AWS\n"+
		"cost. It creates the resource in networking YOU name, and CHANGES NO\n"+
		"NETWORKING itself: no subnet group, no security group, no ingress rule.\n"+
		"So it requires:\n"+
		"  • an EXISTING DB/cache subnet group, and EXISTING security group(s) that\n"+
		"    already allow the cluster to reach the service — if you do not know\n"+
		"    these, ask whoever owns your VPC;\n"+
		"  • AWS permissions to create the service. If your AWS access is limited,\n"+
		"    run `llamacloudctl permissions` to see exactly what to request, or hand\n"+
		"    `llamacloudctl terraform` to whoever owns the account so THEY provision\n"+
		"    it and you pick \"existing endpoint\" instead.", kind)
}

// managedNetworkingInputs is the per-cloud union of collected networking. Exactly
// one field is populated, selected by the cloud the collector dispatched on, so a
// GCP provisioner can never be handed AWS inputs — and an Azure one can never be
// handed either.
type managedNetworkingInputs struct {
	AWS   provision.Networking
	GCP   provision.GCPNetworking
	Azure provision.AzureNetworking
}

// collectManagedNetworking dispatches to the per-cloud collector.
func collectManagedNetworking(p tui.Prompter, c cloud.Cloud, kind string, opts WizardOptions) (managedNetworkingInputs, error) {
	switch c {
	case cloud.Azure:
		net, err := collectAzureManagedNetworking(p, kind, opts)
		return managedNetworkingInputs{Azure: net}, err
	case cloud.GCP:
		net, err := collectGCPManagedNetworking(p, kind, opts)
		return managedNetworkingInputs{GCP: net}, err
	default:
		net, err := collectAWSManagedNetworking(p, kind, opts)
		return managedNetworkingInputs{AWS: net}, err
	}
}

// collectAzureManagedNetworking asks for the EXISTING subscription, resource
// group and per-kind networking the managed resource needs, defaulting each
// prompt to the matching --azure-*/--pg-*/--redis-* flag value so an operator who
// passed the flags just presses enter. The CLI creates none of this networking
// and never creates a subscription or resource group, so an empty answer is a
// hard error naming the flag that supplies it rather than a fallback to anything
// discovered or invented. Prompter errors are returned verbatim — a cancelled
// prompt must abort the wizard, never be swallowed into a "missing networking"
// message.
//
// The prompt SET is per-kind, which is what makes Azure the first cloud whose
// placement inputs differ by service: postgres needs the delegated subnet and the
// private DNS zone it is VNet-integrated into, while redis takes no networking
// parameter at all and instead needs the private endpoint the operator ASSERTS
// already fronts it.
func collectAzureManagedNetworking(p tui.Prompter, kind string, opts WizardOptions) (provision.AzureNetworking, error) {
	mn := opts.ManagedNetworking

	subscription, err := p.Input(fmt.Sprintf("%s Azure subscription id", kind), mn.AzureSubscriptionID)
	if err != nil {
		return provision.AzureNetworking{}, err
	}
	resourceGroup, err := p.Input(fmt.Sprintf("%s Azure resource group", kind), mn.AzureResourceGroup)
	if err != nil {
		return provision.AzureNetworking{}, err
	}

	net := provision.AzureNetworking{
		SubscriptionID: strings.TrimSpace(subscription),
		ResourceGroup:  strings.TrimSpace(resourceGroup),
	}
	missing := []struct {
		value string
		flag  string
	}{
		{net.SubscriptionID, "--azure-subscription-id"},
		{net.ResourceGroup, "--azure-resource-group"},
	}

	if kind == "redis" {
		endpoint, err := p.Input(fmt.Sprintf("%s private endpoint resource id (GroupId redisEnterprise)", kind), mn.AzureRedisPrivateEndpointID)
		if err != nil {
			return provision.AzureNetworking{}, err
		}
		net.RedisPrivateEndpointID = strings.TrimSpace(endpoint)
		missing = append(missing, struct {
			value string
			flag  string
		}{net.RedisPrivateEndpointID, "--redis-private-endpoint-id"})
	} else {
		subnet, err := p.Input(fmt.Sprintf("%s delegated subnet resource id (Microsoft.DBforPostgreSQL/flexibleServers)", kind), mn.AzurePGDelegatedSubnetID)
		if err != nil {
			return provision.AzureNetworking{}, err
		}
		zone, err := p.Input(fmt.Sprintf("%s private DNS zone resource id (.postgres.database.azure.com)", kind), mn.AzurePGPrivateDNSZoneID)
		if err != nil {
			return provision.AzureNetworking{}, err
		}
		net.PGDelegatedSubnetID = strings.TrimSpace(subnet)
		net.PGPrivateDNSZoneID = strings.TrimSpace(zone)
		missing = append(missing,
			struct {
				value string
				flag  string
			}{net.PGDelegatedSubnetID, "--pg-delegated-subnet-id"},
			struct {
				value string
				flag  string
			}{net.PGPrivateDNSZoneID, "--pg-private-dns-zone-id"},
		)
	}

	for _, m := range missing {
		if m.value == "" {
			return provision.AzureNetworking{}, fmt.Errorf("%s: managed provisioning on Azure requires an EXISTING subscription, resource group and networking you name — the installer creates none of it and never creates a subscription or resource group. Supply %s (or answer the prompt); ask whoever owns your VNet if you do not know it", kind, m.flag)
		}
	}
	return net, nil
}

// collectGCPManagedNetworking asks for the EXISTING project, VPC network and
// allocated IP range the managed resource is placed in, defaulting each prompt to
// the matching --gcp-* flag value so an operator who passed the flags just presses
// enter. The CLI creates none of this networking and never creates a project, so
// an empty answer is a hard error naming the flag that supplies it rather than a
// fallback to anything discovered or invented. Prompter errors are returned
// verbatim — a cancelled prompt must abort the wizard, never be swallowed into a
// "missing networking" message.
func collectGCPManagedNetworking(p tui.Prompter, kind string, opts WizardOptions) (provision.GCPNetworking, error) {
	project, err := p.Input(fmt.Sprintf("%s GCP project", kind), opts.ManagedNetworking.GCPProject)
	if err != nil {
		return provision.GCPNetworking{}, err
	}
	network, err := p.Input(fmt.Sprintf("%s VPC network (projects/<project>/global/networks/<name>)", kind), opts.ManagedNetworking.GCPNetwork)
	if err != nil {
		return provision.GCPNetworking{}, err
	}
	ipRange, err := p.Input(fmt.Sprintf("%s allocated IP range name (private services access)", kind), opts.ManagedNetworking.GCPAllocatedIPRange)
	if err != nil {
		return provision.GCPNetworking{}, err
	}

	project, network, ipRange = strings.TrimSpace(project), strings.TrimSpace(network), strings.TrimSpace(ipRange)
	for _, missing := range []struct {
		value string
		flag  string
	}{
		{project, "--gcp-project"},
		{network, "--gcp-vpc-network"},
		{ipRange, "--gcp-allocated-ip-range"},
	} {
		if missing.value == "" {
			return provision.GCPNetworking{}, fmt.Errorf("%s: managed provisioning on GCP requires an EXISTING project and networking you name — the installer creates none and never creates a project. Supply %s (or answer the prompt); ask whoever owns your VPC if you do not know it", kind, missing.flag)
		}
	}
	return provision.GCPNetworking{ProjectID: project, Network: network, AllocatedIPRange: ipRange}, nil
}
