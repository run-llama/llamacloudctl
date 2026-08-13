package install

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/tui"
)

// This file covers the PRESENTATION half of the multi-cloud managed surface: the
// per-cloud resource labels, network notes and provider grouping, plus the Scope
// (GCP project) suffix at each of the three render sites. The AWS wording is
// asserted VERBATIM everywhere so a future cloud cannot quietly reword it.

func TestManagedResourceType_PerCloud(t *testing.T) {
	cases := []struct {
		c    cloud.Cloud
		kind provision.Kind
		want string
	}{
		// AWS labels are frozen — these strings ship today.
		{cloud.AWS, provision.KindPostgres, "RDS PostgreSQL"},
		{cloud.AWS, provision.KindRedis, "ElastiCache Redis"},
		// An unresolved cloud must render the historical AWS labels.
		{cloud.Cloud(""), provision.KindPostgres, "RDS PostgreSQL"},
		{cloud.GCP, provision.KindPostgres, "Cloud SQL (PostgreSQL)"},
		{cloud.GCP, provision.KindRedis, "Memorystore (Redis)"},
		{cloud.GCP, provision.Kind("unknown"), "Managed data service"},
		{cloud.AWS, provision.Kind("unknown"), "Managed data service"},
	}
	for _, tc := range cases {
		if got := managedResourceType(tc.c, tc.kind); got != tc.want {
			t.Errorf("managedResourceType(%q, %q) = %q, want %q", tc.c, tc.kind, got, tc.want)
		}
	}
}

func TestManagedResourceType_GCPNeverNamesAWSServices(t *testing.T) {
	for _, kind := range []provision.Kind{provision.KindPostgres, provision.KindRedis} {
		got := managedResourceType(cloud.GCP, kind)
		for _, banned := range []string{"RDS", "ElastiCache"} {
			if strings.Contains(got, banned) {
				t.Errorf("managedResourceType(gcp, %q) = %q, must not name the AWS service %q", kind, got, banned)
			}
		}
	}
}

func TestManagedNetworkNote_PerCloud(t *testing.T) {
	// The note is now per-KIND as well as per-cloud, because Azure is the first
	// cloud whose placement differs by service. On AWS and GCP it must stay
	// IDENTICAL for both kinds, and byte-identical to the frozen wording.
	const awsNote = "placed in the operator-supplied subnet group + security group(s)"
	const gcpNote = "placed in the operator-supplied VPC network + allocated IP range"
	for _, kind := range []provision.Kind{provision.KindPostgres, provision.KindRedis} {
		if got := managedNetworkNote(cloud.AWS, kind); got != awsNote {
			t.Errorf("managedNetworkNote(aws, %s) = %q, want the frozen AWS wording %q", kind, got, awsNote)
		}
		if got := managedNetworkNote(cloud.Cloud(""), kind); got != awsNote {
			t.Errorf("managedNetworkNote(unresolved, %s) = %q, want the AWS default %q", kind, got, awsNote)
		}
		if got := managedNetworkNote(cloud.GCP, kind); got != gcpNote {
			t.Errorf("managedNetworkNote(gcp, %s) = %q, want %q", kind, got, gcpNote)
		}
	}

	// Azure DIFFERS per kind — that difference is the whole reason for the Kind
	// parameter, so assert it rather than only that the arms exist.
	azPG := managedNetworkNote(cloud.Azure, provision.KindPostgres)
	azRedis := managedNetworkNote(cloud.Azure, provision.KindRedis)
	if azPG == azRedis {
		t.Errorf("azure postgres and redis notes are identical (%q); postgres lands in a delegated subnet, redis is reached through an asserted private endpoint", azPG)
	}
	if !strings.Contains(azPG, "delegated subnet") || !strings.Contains(azPG, "private DNS zone") {
		t.Errorf("azure postgres note = %q, want it to name the delegated subnet + private DNS zone", azPG)
	}
	if !strings.Contains(azRedis, "private endpoint") {
		t.Errorf("azure redis note = %q, want it to name the asserted private endpoint", azRedis)
	}
	for _, note := range []string{azPG, azRedis} {
		for _, banned := range []string{"subnet group", "security group", "VPC network", "allocated IP range"} {
			if strings.Contains(note, banned) {
				t.Errorf("azure note %q must not use the %q vocabulary of another cloud", note, banned)
			}
		}
	}
}

func TestCloudLabel_MatchesWriteTextGroups(t *testing.T) {
	// The labels MUST match the provider-group iterator in WriteText exactly, or the
	// row is silently dropped from the snapshot.
	cases := map[cloud.Cloud]string{
		cloud.AWS:       "AWS",
		cloud.GCP:       "GCP",
		cloud.Azure:     "Azure",
		cloud.Cloud(""): "AWS",
	}
	for c, want := range cases {
		if got := cloudLabel(c); got != want {
			t.Errorf("cloudLabel(%q) = %q, want %q", c, got, want)
		}
	}
}

func TestManagedScopeDetail(t *testing.T) {
	// An empty scope renders NOTHING on every cloud, so AWS lines stay
	// byte-identical.
	for _, c := range []cloud.Cloud{cloud.AWS, cloud.Azure, cloud.GCP, cloud.Cloud("")} {
		if got := managedScopeDetail(c, ""); got != "" {
			t.Errorf("managedScopeDetail(%q, \"\") = %q, want empty so AWS lines are byte-identical", c, got)
		}
	}
	// The NOUN is per-cloud: rendering "in project my-rg" for an Azure resource
	// group would be wrong.
	if got := managedScopeDetail(cloud.GCP, "my-project"); got != " in project my-project" {
		t.Errorf("managedScopeDetail(gcp) = %q, want \" in project my-project\"", got)
	}
	if got := managedScopeDetail(cloud.Azure, "my-rg"); got != " in resource group my-rg" {
		t.Errorf("managedScopeDetail(azure) = %q, want \" in resource group my-rg\"", got)
	}
}

// TestAddManagedResource_GCPRow proves a GCP managed resource records under the GCP
// provider group, carries the project in its detail, and names the GCP service.
func TestAddManagedResource_GCPRow(t *testing.T) {
	inv := &Inventory{}
	inv.addManagedResource(
		cloudLabel(cloud.GCP),
		managedResourceType(cloud.GCP, provision.KindPostgres),
		"prod-llamacloud-pg",
		"us-central1",
		"endpoint 10.24.0.3:5432",
		"my-project",
		scopeNoun(cloud.GCP),
		managedNetworkNote(cloud.GCP, provision.KindPostgres),
	)
	rs := inv.byProvider("GCP")
	if len(rs) != 1 {
		t.Fatalf("byProvider(GCP) = %d rows, want 1 (a wrong provider label drops the row)", len(rs))
	}
	got := rs[0]
	if got.Type != "Cloud SQL (PostgreSQL)" {
		t.Errorf("Type = %q, want %q", got.Type, "Cloud SQL (PostgreSQL)")
	}
	if got.Location != "us-central1" {
		t.Errorf("Location = %q, want us-central1", got.Location)
	}
	if !strings.Contains(got.Detail, "project my-project") {
		t.Errorf("Detail = %q, want it to carry the GCP project", got.Detail)
	}
	if !strings.Contains(got.Detail, "VPC network + allocated IP range") {
		t.Errorf("Detail = %q, want the GCP network note", got.Detail)
	}

	// And it renders under the GCP heading, never the AWS one.
	var b strings.Builder
	inv.WriteText(&b)
	out := b.String()
	if !strings.Contains(out, "\nGCP\n") {
		t.Errorf("WriteText missing the GCP provider heading\n---\n%s", out)
	}
	for _, banned := range []string{"RDS", "ElastiCache"} {
		if strings.Contains(out, banned) {
			t.Errorf("GCP snapshot names the AWS service %q\n---\n%s", banned, out)
		}
	}
}

// TestAddManagedResource_EmptyProviderStaysAWS pins the byte-identical-AWS
// guarantee: the widened signature with an empty provider and empty scope produces
// exactly the row the pre-multi-cloud signature did.
func TestAddManagedResource_EmptyProviderStaysAWS(t *testing.T) {
	inv := &Inventory{}
	inv.addManagedResource("", "RDS PostgreSQL", "pg", "us-east-1", "endpoint h:5432", "", "", "note")
	rs := inv.byProvider("AWS")
	if len(rs) != 1 {
		t.Fatalf("byProvider(AWS) = %d rows, want 1", len(rs))
	}
	if rs[0].Detail != "endpoint h:5432; note" {
		t.Errorf("Detail = %q, want %q (an empty scope must contribute no segment)", rs[0].Detail, "endpoint h:5432; note")
	}
}

// TestRenderPlan_ManagedScope covers render site 1 of 3: the approval-gate plan
// line carries the scope for a scoped provisioner and is unchanged without one.
//
// The scope NOUN follows the install's target CLOUD — a GCP project, an
// Azure resource group — so the scoped cases name their cloud. Before that the
// noun was the hard-coded literal "project", which would have rendered
// "in project my-rg" for an Azure resource group.
func TestRenderPlan_ManagedScope(t *testing.T) {
	v := mkValues(t, nil)

	for _, tc := range []struct {
		c    cloud.Cloud
		want string
	}{
		{cloud.GCP, "ensure managed postgres prod-llamacloud-pg (region us-east-1) in project my-project"},
		{cloud.Azure, "ensure managed postgres prod-llamacloud-pg (region us-east-1) in resource group my-project"},
	} {
		scoped := newFakePostgresProvisioner()
		scoped.scope = "my-project"
		var b strings.Builder
		renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "prod", Cloud: tc.c}, v, &ProvisionPlan{Managed: []provision.Provisioner{scoped}})
		if got := b.String(); !strings.Contains(got, tc.want) {
			t.Errorf("%s scoped plan line missing %q\n---\n%s", tc.c, tc.want, got)
		}
	}

	// Unscoped (AWS) stays byte-identical: no trailing project segment.
	var b2 strings.Builder
	renderPlan(&b2, Options{ReleaseName: "llamacloud", Namespace: "prod"}, v, &ProvisionPlan{Managed: []provision.Provisioner{newFakePostgresProvisioner()}})
	out2 := b2.String()
	if !strings.Contains(out2, "ensure managed postgres prod-llamacloud-pg (region us-east-1)\n") {
		t.Errorf("unscoped plan line changed shape\n---\n%s", out2)
	}
	if strings.Contains(out2, "in project") {
		t.Errorf("unscoped plan line gained a project segment\n---\n%s", out2)
	}
}

// TestRunProvisioning_GCPInventoryRow drives the REAL install call site (not
// addManagedResource directly) with cloud gcp, proving runProvisioning records the
// resource under the GCP provider group with the GCP service label, the project
// scope and the GCP network note — never the AWS ones.
func TestRunProvisioning_GCPInventoryRow(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	fp.identifier = "prod-llamacloud-pg"
	fp.region = "us-central1"
	fp.scope = "my-project"
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	inv := &Inventory{}
	opts := Options{
		Cloud:          cloud.GCP,
		Namespace:      "prod",
		Out:            &strings.Builder{},
		OutputPath:     filepath.Join(t.TempDir(), "v.yaml"),
		OutputExplicit: true,
		inventory:      inv,
		accountGuard:   noopAccountGuard,
	}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		t.Fatalf("runProvisioning: %v", err)
	}

	if aws := inv.byProvider("AWS"); len(aws) != 0 {
		t.Errorf("a GCP install recorded %d AWS rows, want 0: %+v", len(aws), aws)
	}
	rs := inv.byProvider("GCP")
	if len(rs) != 1 {
		t.Fatalf("expected 1 GCP resource recorded, got %d: %+v", len(rs), rs)
	}
	got := rs[0]
	if got.Type != "Cloud SQL (PostgreSQL)" {
		t.Errorf("Type = %q, want Cloud SQL (PostgreSQL)", got.Type)
	}
	if got.Location != "us-central1" {
		t.Errorf("Location = %q, want us-central1", got.Location)
	}
	if !strings.Contains(got.Detail, "project my-project") {
		t.Errorf("Detail = %q, want the project scope", got.Detail)
	}
	if !strings.Contains(got.Detail, "VPC network + allocated IP range") {
		t.Errorf("Detail = %q, want the GCP network note", got.Detail)
	}
	if strings.Contains(got.Detail, "subnet group") {
		t.Errorf("Detail = %q, must not carry the AWS network note", got.Detail)
	}
}

// TestManagedNetworkingDetail_GCPProvisioners proves the approval gate renders the
// operator-supplied GCP networking for BOTH GCP provisioners, instead of the empty
// string the default branch returns for an unrecognized implementation.
func TestManagedNetworkingDetail_GCPProvisioners(t *testing.T) {
	net := provision.GCPNetworking{
		ProjectID:        "op-project",
		Network:          "projects/op-project/global/networks/op-vpc",
		AllocatedIPRange: "op-psa-range",
	}
	cc := provision.ClientConfig{}
	want := " in network projects/op-project/global/networks/op-vpc (allocated range op-psa-range)"

	for name, p := range map[string]provision.Provisioner{
		"cloudsql":    provision.NewCloudSQLProvisioner("prod", "us-central1", net, cc),
		"memorystore": provision.NewMemorystoreProvisioner("prod", "us-central1", net, cc),
	} {
		if got := managedNetworkingDetail(p); got != want {
			t.Errorf("%s: managedNetworkingDetail = %q, want %q", name, got, want)
		}
	}

	// A zero GCPNetworking (teardown-only construction) renders nothing.
	bare := provision.NewCloudSQLProvisioner("prod", "us-central1", provision.GCPNetworking{}, cc)
	if got := managedNetworkingDetail(bare); got != "" {
		t.Errorf("zero networking rendered %q, want empty", got)
	}
}

// TestRenderPlan_GCPProvisionerFullLine is the end-to-end approval-gate assertion:
// a real GCP provisioner's plan line carries the project scope AND the GCP
// networking, in the GCP vocabulary.
func TestRenderPlan_GCPProvisionerFullLine(t *testing.T) {
	v := mkValues(t, nil)
	p := provision.NewCloudSQLProvisioner("prod", "us-central1", provision.GCPNetworking{
		ProjectID:        "op-project",
		Network:          "op-vpc",
		AllocatedIPRange: "op-psa-range",
	}, provision.ClientConfig{})

	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "prod", Cloud: cloud.GCP}, v, &ProvisionPlan{Managed: []provision.Provisioner{p}})
	out := b.String()

	want := "ensure managed postgres prod-llamacloud-pg (region us-central1) in project op-project in network op-vpc (allocated range op-psa-range)"
	if !strings.Contains(out, want) {
		t.Errorf("GCP plan line missing scope and/or networking\nwant: %s\n---\n%s", want, out)
	}
	for _, banned := range []string{"subnet group", "security group"} {
		if strings.Contains(out, banned) {
			t.Errorf("GCP plan line uses the AWS networking vocabulary %q\n---\n%s", banned, out)
		}
	}
}

// gcpWizardAnswers is the full interactive answer map for a --cloud gcp run that
// picks "provision managed" for both data services.
func gcpWizardAnswers() map[string]any {
	return map[string]any{
		"Chart version (empty = latest published)": "",

		"License source":      "paste JWT",
		"License JWT":         "jwt-token",
		"Authentication mode": "oidc",
		"OIDC discovery URL":  "https://id.example.com",
		"OIDC client ID":      "client-1",
		"OIDC client secret":  "shhh",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk-openai",
		"Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)": true,
		"Object storage provider":                                                                                "gcp",
		"Existing bucket name (applied to all bucket purposes)":                                                  "acme-storage",
		"gcp storage endpoint (JCLOUDS_ENDPOINT)":                                                                "https://storage.googleapis.com",
		"Storage account / access identity (JCLOUDS_IDENTITY)":                                                   "acme",
		"Storage access key / credential (JCLOUDS_CREDENTIAL)":                                                   "gcp-key",
		"Workload-identity GCP service account email to annotate on the platform ServiceAccounts (empty = none)": "",

		"postgres: how should it be provided?": string(modeManaged),
		"redis: how should it be provided?":    string(modeManaged),
		// Inert for the managed answers above; consumed by the tests below that
		// flip a service to in-cluster, which now reach the shared StorageClass
		// step. Empty = the cluster default, as before.
		dbStorageClassPrompt: "",

		// The three GCP networking prompts, for both services.
		"postgres GCP project": "op-project",
		"postgres VPC network (projects/<project>/global/networks/<name>)": "projects/op-project/global/networks/op-vpc",
		"postgres allocated IP range name (private services access)":       "op-psa-range",
		"redis GCP project": "op-project",
		"redis VPC network (projects/<project>/global/networks/<name>)": "projects/op-project/global/networks/op-vpc",
		"redis allocated IP range name (private services access)":       "op-psa-range",
	}
}

// TestWizard_GCPOffersManagedAndHandoff proves the option list is driven by
// cloud.SupportsManaged: gcp is offered BOTH managed and handoff.
func TestWizard_GCPOffersManagedAndHandoff(t *testing.T) {
	answers := gcpWizardAnswers()
	answers["postgres: how should it be provided?"] = string(modeInCluster)
	answers["redis: how should it be provided?"] = string(modeInCluster)

	p := newRecordingPrompter(tui.NewNonInteractive(answers))
	v := mkValues(t, nil)
	if _, err := RunWizard(p, v, WizardOptions{Cloud: cloud.GCP, Namespace: "acme"}); err != nil {
		t.Fatalf("the gcp wizard must complete normally, got: %v", err)
	}

	opts, ok := p.selects["postgres: how should it be provided?"]
	if !ok {
		t.Fatal("the postgres mode prompt was never shown")
	}
	var sawManaged, sawHandoff bool
	for _, o := range opts {
		switch o {
		case string(modeManaged):
			sawManaged = true
		case string(modeHandoff):
			sawHandoff = true
		}
	}
	if !sawManaged {
		t.Errorf("gcp must be offered \"provision managed\": %v", opts)
	}
	if !sawHandoff {
		t.Errorf("gcp must be offered the terraform handoff: %v", opts)
	}
}

// TestWizard_GCPManagedRecordsGCPProvisioners is the anti-regression for the live
// bug: a --cloud gcp run answering "provision managed" must record Cloud SQL and
// Memorystore provisioners, NEVER the AWS ones, carrying the operator-supplied
// project/network/range.
func TestWizard_GCPManagedRecordsGCPProvisioners(t *testing.T) {
	p := tui.NewNonInteractive(gcpWizardAnswers())
	v := mkValues(t, nil)
	plan, err := RunWizard(p, v, WizardOptions{Cloud: cloud.GCP, Namespace: "acme", Region: "us-central1"})
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if len(plan.Managed) != 2 {
		t.Fatalf("plan.Managed = %d provisioners, want 2", len(plan.Managed))
	}

	var sawPG, sawRedis bool
	for _, pr := range plan.Managed {
		switch c := pr.(type) {
		case *provision.CloudSQLProvisioner:
			sawPG = true
			if c.Params.GCP.ProjectID != "op-project" {
				t.Errorf("Cloud SQL project = %q, want op-project", c.Params.GCP.ProjectID)
			}
			if c.Params.GCP.Network != "projects/op-project/global/networks/op-vpc" {
				t.Errorf("Cloud SQL network = %q, want the answered network", c.Params.GCP.Network)
			}
			if c.Params.GCP.AllocatedIPRange != "op-psa-range" {
				t.Errorf("Cloud SQL allocated range = %q, want op-psa-range", c.Params.GCP.AllocatedIPRange)
			}
			if c.Scope() != "op-project" {
				t.Errorf("Scope() = %q, want op-project", c.Scope())
			}
		case *provision.MemorystoreProvisioner:
			sawRedis = true
			if c.Params.GCP.ProjectID != "op-project" {
				t.Errorf("Memorystore project = %q, want op-project", c.Params.GCP.ProjectID)
			}
		default:
			t.Errorf("recorded a %T on a --cloud gcp run — the wizard built the WRONG cloud's provisioner", pr)
		}
	}
	if !sawPG || !sawRedis {
		t.Errorf("want one Cloud SQL and one Memorystore provisioner, got pg=%v redis=%v", sawPG, sawRedis)
	}
}

// TestWizard_ManagedOffAWSRefusesUnsetRegion proves the region refusal lands
// INSIDE recordManagedProvisioner — before the approval gate and before any cloud
// call — so nothing is recorded on plan.Managed and nothing is created.
//
// Before multi-cloud managed support this run silently recorded a provisioner addressed as
// "us-east-1", which is not a region on GCP or Azure, and the operator found out
// from a cloud-side API error deep inside Ensure, long after they had approved.
// A populated AWS environment must NOT rescue it.
func TestWizard_ManagedOffAWSRefusesUnsetRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "ap-south-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

	answers := gcpWizardAnswers()
	answers["redis: how should it be provided?"] = string(modeInCluster)

	p := tui.NewNonInteractive(answers)
	v := mkValues(t, nil)
	plan, err := RunWizard(p, v, WizardOptions{
		Cloud:     cloud.GCP,
		Namespace: "acme",
		// Region deliberately unset.
		ManagedNetworking: ManagedNetworking{
			GCPProject:          "op-project",
			GCPNetwork:          "projects/op-project/global/networks/op-vpc",
			GCPAllocatedIPRange: "op-range",
		},
	})
	if err == nil {
		t.Fatal("a managed install off AWS with no --region must abort, got nil")
	}
	if !strings.Contains(err.Error(), "--region") {
		t.Errorf("error = %q, want it to name the --region flag", err)
	}
	if !strings.Contains(err.Error(), "GCP requires an explicit region") {
		t.Errorf("error = %q, want it to state GCP's explicit-region requirement", err)
	}
	if strings.Contains(err.Error(), "ap-south-1") {
		t.Errorf("error = %q, must not have resolved a region from the AWS environment", err)
	}
	// Nothing was recorded: the refusal is before the approval gate.
	if plan != nil && len(plan.Managed) != 0 {
		t.Errorf("plan.Managed = %d, want 0 — nothing may be recorded when the region is refused", len(plan.Managed))
	}
}

// TestWizard_GCPManagedUsesFlagDefaults proves the --gcp-* flags are the prompt
// DEFAULTS, so a flags-only operator just presses enter.
func TestWizard_GCPManagedUsesFlagDefaults(t *testing.T) {
	answers := gcpWizardAnswers()
	// Omitting the answers makes the prompter return the supplied DEFAULT — which
	// is what an operator pressing enter on a flags-only run produces.
	delete(answers, "postgres GCP project")
	delete(answers, "postgres VPC network (projects/<project>/global/networks/<name>)")
	delete(answers, "postgres allocated IP range name (private services access)")
	answers["redis: how should it be provided?"] = string(modeInCluster)

	p := tui.NewNonInteractive(answers)
	v := mkValues(t, nil)
	plan, err := RunWizard(p, v, WizardOptions{
		Cloud:     cloud.GCP,
		Namespace: "acme",
		// Managed provisioning off AWS REQUIRES an explicit region:
		// there is no fallback convention outside AWS, so recordManagedProvisioner
		// refuses an unset one before the approval gate.
		Region: "us-central1",
		ManagedNetworking: ManagedNetworking{
			GCPProject:          "flag-project",
			GCPNetwork:          "flag-network",
			GCPAllocatedIPRange: "flag-range",
		},
	})
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if len(plan.Managed) != 1 {
		t.Fatalf("plan.Managed = %d, want 1", len(plan.Managed))
	}
	c, ok := plan.Managed[0].(*provision.CloudSQLProvisioner)
	if !ok {
		t.Fatalf("recorded a %T, want *provision.CloudSQLProvisioner", plan.Managed[0])
	}
	if c.Params.GCP.ProjectID != "flag-project" || c.Params.GCP.Network != "flag-network" || c.Params.GCP.AllocatedIPRange != "flag-range" {
		t.Errorf("prompt defaults did not come from the --gcp-* flags: %+v", c.Params.GCP)
	}
}

// TestCollectGCPManagedNetworking_EmptyAnswerIsHardError proves an empty answer to
// any of the three prompts is a hard error naming the corresponding flag — never a
// fallback to anything discovered or invented.
func TestCollectGCPManagedNetworking_EmptyAnswerIsHardError(t *testing.T) {
	base := map[string]any{
		"postgres GCP project": "op-project",
		"postgres VPC network (projects/<project>/global/networks/<name>)": "op-vpc",
		"postgres allocated IP range name (private services access)":       "op-psa-range",
	}
	cases := map[string]string{
		"postgres GCP project": "--gcp-project",
		"postgres VPC network (projects/<project>/global/networks/<name>)": "--gcp-vpc-network",
		"postgres allocated IP range name (private services access)":       "--gcp-allocated-ip-range",
	}
	for prompt, flag := range cases {
		answers := map[string]any{}
		for k, v := range base {
			answers[k] = v
		}
		answers[prompt] = ""

		_, err := collectGCPManagedNetworking(tui.NewNonInteractive(answers), "postgres", WizardOptions{})
		if err == nil {
			t.Errorf("empty %q must be a hard error", prompt)
			continue
		}
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("empty %q: error = %q, want it to name %s", prompt, err, flag)
		}
	}
}

// TestCollectGCPManagedNetworking_PrompterErrorPropagates proves a CANCELLED prompt
// aborts with the prompter's own error rather than being swallowed into the
// missing-networking message — otherwise a cancel would read as a config mistake.
func TestCollectGCPManagedNetworking_PrompterErrorPropagates(t *testing.T) {
	sentinel := errors.New("prompt cancelled")
	_, err := collectGCPManagedNetworking(&erroringPrompter{err: sentinel}, "postgres", WizardOptions{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the prompter's own error verbatim", err)
	}
	if strings.Contains(fmt.Sprint(err), "--gcp-project") {
		t.Errorf("a cancelled prompt was swallowed into the missing-networking message: %v", err)
	}
}

// erroringPrompter fails every Input, standing in for a cancelled prompt.
type erroringPrompter struct {
	tui.Prompter
	err error
}

func (e *erroringPrompter) Input(string, string) (string, error) { return "", e.err }

// TestManagedServiceNote_GCPStatesTLSPosture proves the operator is TOLD about the
// Memorystore no-TLS posture at the prompt, not just in a code comment.
func TestManagedServiceNote_GCPStatesTLSPosture(t *testing.T) {
	note := managedServiceNote(cloud.GCP, "redis")
	for _, want := range []string{"AUTH", "DISABLED", "redis://", "rediss://", "no field to carry that CA"} {
		if !strings.Contains(note, want) {
			t.Errorf("the GCP redis note must mention %q:\n%s", want, note)
		}
	}
	// The postgres note carries no redis TLS paragraph.
	if strings.Contains(managedServiceNote(cloud.GCP, "postgres"), "Memorystore security") {
		t.Error("the postgres note must not carry the Memorystore TLS paragraph")
	}
	// AWS wording is unchanged and never mentions GCP services.
	aws := managedServiceNote(cloud.AWS, "redis")
	if !strings.Contains(aws, "Managed provisioning for redis (AWS)") {
		t.Errorf("AWS note wording changed:\n%s", aws)
	}
	for _, banned := range []string{"Cloud SQL", "Memorystore"} {
		if strings.Contains(aws, banned) {
			t.Errorf("the AWS note names the GCP service %q:\n%s", banned, aws)
		}
	}
}

// TestApplyTerraformOutputs_GCPIdentityAndProvider proves the round-trip consumer
// is cloud-aware: on GCP the frozen irsa_role_arn output — which carries the
// cloud's identity ANALOG, a GSA email — is wired under
// iam.gke.io/gcp-service-account, never eks.amazonaws.com/role-arn, and the
// storage provider is "gcp". Writing a GSA email under the EKS annotation would
// produce a silently broken install.
func TestApplyTerraformOutputs_GCPIdentityAndProvider(t *testing.T) {
	v := mkValues(t, nil)
	outs := infra.TerraformOutputs{
		BucketNames: []string{"acme-storage"},
		IRSARoleARN: "llamacloud@op-project.iam.gserviceaccount.com",
	}
	var b strings.Builder
	applyTerraformOutputs(v, cloud.GCP, outs, &b)

	if got := getString(v, "config.storageBuckets.provider"); got != "gcp" {
		t.Errorf("config.storageBuckets.provider = %q, want gcp", got)
	}

	// EVERY storage workload's ServiceAccount carries the GKE annotation.
	for _, w := range irsaWorkloads {
		raw, ok := v.Get(w + ".serviceAccountAnnotations")
		if !ok {
			t.Errorf("%s: no serviceAccountAnnotations written", w)
			continue
		}
		ann, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s: annotations are %T, want a map", w, raw)
			continue
		}
		if _, bad := ann["eks.amazonaws.com/role-arn"]; bad {
			t.Errorf("%s: a GCP run wrote the AWS IRSA annotation: %v", w, ann)
		}
		if got := ann["iam.gke.io/gcp-service-account"]; got != "llamacloud@op-project.iam.gserviceaccount.com" {
			t.Errorf("%s: iam.gke.io/gcp-service-account = %v, want the GSA email", w, got)
		}
	}

	// The summary names the identity in the cloud's own vocabulary and echoes no
	// secret.
	out := b.String()
	if !strings.Contains(out, "GCP service account email") {
		t.Errorf("summary must name the GCP identity label:\n%s", out)
	}
	if !strings.Contains(out, "not the object-storage") {
		t.Errorf("summary must say the outputs carry no object-storage endpoint/credential:\n%s", out)
	}
}

// TestApplyTerraformOutputs_AWSUnchanged is the byte-level guard on the shipped
// path: the AWS branch still writes provider "aws" and the EKS IRSA annotation,
// and prints no s3proxy note (AWS needs no shim).
func TestApplyTerraformOutputs_AWSUnchanged(t *testing.T) {
	v := mkValues(t, nil)
	outs := infra.TerraformOutputs{
		BucketNames: []string{"acme-storage"},
		IRSARoleARN: "arn:aws:iam::123456789012:role/llamacloud",
	}
	var b strings.Builder
	applyTerraformOutputs(v, cloud.AWS, outs, &b)

	if got := getString(v, "config.storageBuckets.provider"); got != "aws" {
		t.Errorf("config.storageBuckets.provider = %q, want aws", got)
	}
	raw, ok := v.Get("backend.serviceAccountAnnotations")
	if !ok {
		t.Fatal("no serviceAccountAnnotations written on the AWS path")
	}
	ann := raw.(map[string]any)
	if got := ann["eks.amazonaws.com/role-arn"]; got != "arn:aws:iam::123456789012:role/llamacloud" {
		t.Errorf("eks.amazonaws.com/role-arn = %v, want the role ARN", got)
	}
	if _, bad := ann["iam.gke.io/gcp-service-account"]; bad {
		t.Errorf("an AWS run wrote the GKE annotation: %v", ann)
	}
	if strings.Contains(b.String(), "object-storage\nendpoint") {
		t.Errorf("AWS needs no s3proxy note:\n%s", b.String())
	}
}

// TestRun_GCPManagedDryRunComposedShape is the end-to-end composed verification
// for the GCP managed path: a --dry-run install with --cloud gcp, driven through
// the real wizard, must render the approval-gate plan with BOTH ensure-managed
// lines carrying the project, network and allocated range; create NOTHING; and
// echo no secret anywhere in its output.
func TestRun_GCPManagedDryRunComposedShape(t *testing.T) {
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "prod",
		Cloud:      cloud.GCP,
		Region:     "us-central1",
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   tui.NewNonInteractive(gcpWizardAnswers()),
		ManagedNetworking: ManagedNetworking{
			GCPProject:          "op-project",
			GCPNetwork:          "projects/op-project/global/networks/op-vpc",
			GCPAllocatedIPRange: "op-psa-range",
		},
	})
	if err != nil {
		t.Fatalf("gcp managed dry run: %v\n%s", err, out.String())
	}
	got := out.String()

	// Both services render with the project scope AND the GCP networking.
	for _, want := range []string{
		"ensure managed postgres prod-llamacloud-pg (region us-central1) in project op-project in network projects/op-project/global/networks/op-vpc (allocated range op-psa-range)",
		"ensure managed redis prod-llamacloud-redis (region us-central1) in project op-project in network projects/op-project/global/networks/op-vpc (allocated range op-psa-range)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run plan missing:\n  %s\n---\n%s", want, got)
		}
	}
	// AWS vocabulary must not appear on a GCP plan line.
	for _, banned := range []string{"subnet group", "security group"} {
		if strings.Contains(got, banned) {
			t.Errorf("gcp dry run used the AWS networking vocabulary %q\n---\n%s", banned, got)
		}
	}
	// A dry run creates nothing — no inventory snapshot is rendered.
	if strings.Contains(got, "The install stood up the following resources") {
		t.Errorf("a --dry-run rendered a resource snapshot, so something was created\n---\n%s", got)
	}
	// No MANAGED data-service credential is echoed. Nothing was provisioned, so no
	// Cloud SQL root password or Memorystore AUTH string can appear, and the
	// object-storage credential is redacted by the values printer.
	for _, secret := range []string{"gcp-key", "svc-minted-auth"} {
		if strings.Contains(got, secret) {
			t.Errorf("dry-run output echoed the secret %q\n---\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "***REDACTED***") {
		t.Errorf("expected the values dump to redact the object-storage credential\n---\n%s", got)
	}
	// NOTE: the license JWT is NOT redacted in the stdout values dump — the
	// redaction vocabulary (internal/values secretKeyTokens) has no "key"/"license"
	// token, so config.license.key prints in full. That is pre-existing and
	// cloud-neutral (it behaves identically on AWS), so it is deliberately NOT
	// asserted here; it is recorded as a separate finding rather than silently
	// widened by this ticket.
}

// azureWizardAnswers is the full interactive answer map for a --cloud azure run
// that picks "provision managed" for both data services. Note the prompt SET
// differs per kind — postgres is asked for its delegated subnet and private DNS
// zone, redis for its private endpoint — which is what makes Azure the first
// cloud whose placement inputs are per-service.
func azureWizardAnswers() map[string]any {
	return map[string]any{
		"Chart version (empty = latest published)": "",

		"License source":      "paste JWT",
		"License JWT":         "jwt-token",
		"Authentication mode": "oidc",
		"OIDC discovery URL":  "https://id.example.com",
		"OIDC client ID":      "client-1",
		"OIDC client secret":  "shhh",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk-openai",
		"Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)": true,
		"Object storage provider":                                                                                 "azure",
		"Existing bucket name (applied to all bucket purposes)":                                                   "acme-storage",
		"azure storage endpoint (JCLOUDS_ENDPOINT)":                                                               "https://acme.blob.core.windows.net",
		"Storage account / access identity (JCLOUDS_IDENTITY)":                                                    "acme",
		"Storage access key / credential (JCLOUDS_CREDENTIAL)":                                                    "azure-key",
		"Workload-identity managed-identity client ID to annotate on the platform ServiceAccounts (empty = none)": "",

		"postgres: how should it be provided?": string(modeManaged),
		"redis: how should it be provided?":    string(modeManaged),
		// Inert for the managed answers above; consumed by the tests below that
		// flip a service to in-cluster, which now reach the shared StorageClass
		// step. Empty = the cluster default, as before.
		dbStorageClassPrompt: "",

		"postgres Azure subscription id": "sub-1",
		"postgres Azure resource group":  "op-rg",
		"postgres delegated subnet resource id (Microsoft.DBforPostgreSQL/flexibleServers)": "/subscriptions/sub-1/resourceGroups/op-rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/pg",
		"postgres private DNS zone resource id (.postgres.database.azure.com)":              "/subscriptions/sub-1/resourceGroups/op-rg/providers/Microsoft.Network/privateDnsZones/priv.postgres.database.azure.com",

		"redis Azure subscription id":                                  "sub-1",
		"redis Azure resource group":                                   "op-rg",
		"redis private endpoint resource id (GroupId redisEnterprise)": "/subscriptions/sub-1/resourceGroups/op-rg/providers/Microsoft.Network/privateEndpoints/redis-pe",
	}
}

// TestWizard_AzureManagedRecordsAzureProvisioners is the anti-regression twin of
// the GCP case: a --cloud azure run answering "provision managed" must record
// PostgresFlexible and ManagedRedis provisioners, NEVER the RDS/ElastiCache ones,
// carrying the operator-supplied resource group and networking.
func TestWizard_AzureManagedRecordsAzureProvisioners(t *testing.T) {
	p := tui.NewNonInteractive(azureWizardAnswers())
	v := mkValues(t, nil)
	plan, err := RunWizard(p, v, WizardOptions{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus"})
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if len(plan.Managed) != 2 {
		t.Fatalf("plan.Managed = %d provisioners, want 2", len(plan.Managed))
	}

	var sawPG, sawRedis bool
	for _, pr := range plan.Managed {
		switch c := pr.(type) {
		case *provision.PostgresFlexibleProvisioner:
			sawPG = true
			if c.Params.Azure.SubscriptionID != "sub-1" || c.Params.Azure.ResourceGroup != "op-rg" {
				t.Errorf("PG azure scope = %+v, want sub-1/op-rg", c.Params.Azure)
			}
			if !strings.HasSuffix(c.Params.Azure.PGDelegatedSubnetID, "/subnets/pg") {
				t.Errorf("PG delegated subnet = %q, want the answered subnet", c.Params.Azure.PGDelegatedSubnetID)
			}
			if !strings.Contains(c.Params.Azure.PGPrivateDNSZoneID, "postgres.database.azure.com") {
				t.Errorf("PG private DNS zone = %q, want the answered zone", c.Params.Azure.PGPrivateDNSZoneID)
			}
			if c.Scope() != "op-rg" {
				t.Errorf("Scope() = %q, want op-rg (the resource group)", c.Scope())
			}
		case *provision.ManagedRedisProvisioner:
			sawRedis = true
			if c.Params.Azure.ResourceGroup != "op-rg" {
				t.Errorf("redis resource group = %q, want op-rg", c.Params.Azure.ResourceGroup)
			}
			if !strings.HasSuffix(c.Params.Azure.RedisPrivateEndpointID, "/privateEndpoints/redis-pe") {
				t.Errorf("redis private endpoint = %q, want the answered endpoint", c.Params.Azure.RedisPrivateEndpointID)
			}
			if c.Scope() != "op-rg" {
				t.Errorf("Scope() = %q, want op-rg", c.Scope())
			}
		default:
			t.Errorf("recorded a %T on a --cloud azure run — the wizard built the WRONG cloud's provisioner", pr)
		}
	}
	if !sawPG || !sawRedis {
		t.Errorf("want one PostgresFlexible and one ManagedRedis provisioner, got pg=%v redis=%v", sawPG, sawRedis)
	}
}

// TestWizard_AzureManagedRefusesEmptyNetworking: the CLI creates no networking,
// so an EMPTY answer to any Azure networking prompt is a hard error naming the
// corresponding flag — never a fallback to anything discovered or invented.
func TestWizard_AzureManagedRefusesEmptyNetworking(t *testing.T) {
	cases := map[string]string{
		"postgres Azure subscription id": "--azure-subscription-id",
		"postgres Azure resource group":  "--azure-resource-group",
		"postgres delegated subnet resource id (Microsoft.DBforPostgreSQL/flexibleServers)": "--pg-delegated-subnet-id",
		"postgres private DNS zone resource id (.postgres.database.azure.com)":              "--pg-private-dns-zone-id",
	}
	for prompt, wantFlag := range cases {
		t.Run(wantFlag, func(t *testing.T) {
			answers := azureWizardAnswers()
			answers[prompt] = ""

			_, err := RunWizard(tui.NewNonInteractive(answers), mkValues(t, nil),
				WizardOptions{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus"})
			if err == nil {
				t.Fatalf("an empty %q must abort the wizard", prompt)
			}
			if !strings.Contains(err.Error(), wantFlag) {
				t.Errorf("error = %q, want it to name %s", err, wantFlag)
			}
		})
	}

	t.Run("--redis-private-endpoint-id", func(t *testing.T) {
		answers := azureWizardAnswers()
		answers["postgres: how should it be provided?"] = string(modeInCluster)
		answers["redis private endpoint resource id (GroupId redisEnterprise)"] = ""

		_, err := RunWizard(tui.NewNonInteractive(answers), mkValues(t, nil),
			WizardOptions{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus"})
		if err == nil {
			t.Fatal("an empty redis private endpoint must abort the wizard")
		}
		if !strings.Contains(err.Error(), "--redis-private-endpoint-id") {
			t.Errorf("error = %q, want it to name --redis-private-endpoint-id", err)
		}
	})
}

// TestWizard_AzureManagedPropagatesPrompterError: a CANCELLED prompt must abort
// with the PROMPTER's own error, never be swallowed into the missing-networking
// message — the operator cancelled, they did not fail to supply a flag.
func TestWizard_AzureManagedPropagatesPrompterError(t *testing.T) {
	answers := azureWizardAnswers()
	// Omitting an answer from a non-interactive prompter makes Input return its
	// default; instead delete every key so the prompter errors on the unknown
	// prompt, which is the cancellation shape.
	delete(answers, "postgres Azure subscription id")

	_, err := RunWizard(errorOnPromptPrompter{tui.NewNonInteractive(answers), "postgres Azure subscription id"},
		mkValues(t, nil), WizardOptions{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus"})
	if err == nil {
		t.Fatal("a cancelled prompt must abort the wizard")
	}
	if !strings.Contains(err.Error(), "prompt cancelled") {
		t.Errorf("error = %q, want the prompter's own error verbatim", err)
	}
	if strings.Contains(err.Error(), "--azure-subscription-id") {
		t.Errorf("error = %q, must NOT be the missing-networking message — the operator cancelled", err)
	}
}

// errorOnPromptPrompter fails a NAMED prompt with a cancellation error and
// delegates every other prompt, so a test can drive the cancelled-prompt path at
// one specific question.
type errorOnPromptPrompter struct {
	tui.Prompter
	failOn string
}

func (p errorOnPromptPrompter) Input(label, def string) (string, error) {
	if label == p.failOn {
		return "", errors.New("prompt cancelled")
	}
	return p.Prompter.Input(label, def)
}

// TestManagedServiceNote_AzureTLSPosture: the azure POSTGRES note must state
// plainly that the connection is encrypted but the certificate unverified, and
// that this cannot be changed from values. The azure REDIS note must NOT carry
// it, and the AWS and GCP notes must be byte-identical to before.
func TestManagedServiceNote_AzureTLSPosture(t *testing.T) {
	pgNote := managedServiceNote(cloud.Azure, "postgres")
	for _, want := range []string{
		"connection IS encrypted",
		"CERTIFICATE",
		"NOT VERIFIED",
		"no sslmode key",
		"ssl='prefer'",
	} {
		if !strings.Contains(pgNote, want) {
			t.Errorf("azure postgres note missing %q\n---\n%s", want, pgNote)
		}
	}
	// The polarity difference from the GCP Memorystore case must be stated, not
	// left for the reader to infer: this one is NOT plaintext.
	if !strings.Contains(pgNote, "NOT plaintext") {
		t.Errorf("azure postgres note must distinguish itself from the GCP plaintext case\n---\n%s", pgNote)
	}

	redisNote := managedServiceNote(cloud.Azure, "redis")
	if strings.Contains(redisNote, "sslmode") || strings.Contains(redisNote, "ssl='prefer'") {
		t.Errorf("the azure REDIS note must not carry the postgres TLS paragraph\n---\n%s", redisNote)
	}
	// It must still state the AMR reachability hazard.
	if !strings.Contains(redisNote, "UNREACHABLE") {
		t.Errorf("azure redis note must state the private-endpoint reachability hazard\n---\n%s", redisNote)
	}

	// AWS and GCP are untouched.
	if !strings.Contains(managedServiceNote(cloud.AWS, "postgres"), "Managed provisioning for postgres (AWS)") {
		t.Error("the AWS note changed shape")
	}
	if !strings.Contains(managedServiceNote(cloud.GCP, "redis"), "NOTE on Memorystore security") {
		t.Error("the GCP redis note changed shape")
	}
}

// TestManagedNetworkingDetail_AzureArms: the approval-gate plan line renders the
// REAL Azure inputs rather than the empty string the default branch returns, so
// the operator confirms what was collected instead of trusting the flags they
// think they passed.
func TestManagedNetworkingDetail_AzureArms(t *testing.T) {
	net := provision.AzureNetworking{
		SubscriptionID:         "sub-1",
		ResourceGroup:          "op-rg",
		PGDelegatedSubnetID:    "/subs/pg-subnet",
		PGPrivateDNSZoneID:     "/zones/pg-zone",
		RedisPrivateEndpointID: "/eps/redis-pe",
	}
	pg := managedNetworkingDetail(provision.NewPostgresFlexibleProvisioner("acme", "eastus", net, provision.ClientConfig{}))
	for _, want := range []string{"/subs/pg-subnet", "/zones/pg-zone", "delegated subnet"} {
		if !strings.Contains(pg, want) {
			t.Errorf("azure postgres networking detail = %q, want it to contain %q", pg, want)
		}
	}

	rd := managedNetworkingDetail(provision.NewManagedRedisProvisioner("acme", "eastus", net, provision.ClientConfig{}))
	if !strings.Contains(rd, "/eps/redis-pe") {
		t.Errorf("azure redis networking detail = %q, want it to name the private endpoint", rd)
	}
	// It must read as an ASSERTION — it is the one value the CLI neither passes to
	// a create call nor verifies.
	if !strings.Contains(rd, "asserted") {
		t.Errorf("azure redis networking detail = %q, want it to mark the endpoint as asserted", rd)
	}

	// A zero AzureNetworking (teardown-only construction) renders nothing, so the
	// default-branch plan assertions still hold.
	for _, pr := range []provision.Provisioner{
		provision.NewPostgresFlexibleProvisioner("acme", "eastus", provision.AzureNetworking{}, provision.ClientConfig{}),
		provision.NewManagedRedisProvisioner("acme", "eastus", provision.AzureNetworking{}, provision.ClientConfig{}),
	} {
		if got := managedNetworkingDetail(pr); got != "" {
			t.Errorf("%T with zero networking rendered %q, want the empty string", pr, got)
		}
	}
}

// TestManagedResourceType_Azure pins the Azure product names as Azure spells
// them, and proves no AWS/GCP service name leaks onto an Azure row.
func TestManagedResourceType_Azure(t *testing.T) {
	cases := map[provision.Kind]string{
		provision.KindPostgres:  "Azure Database for PostgreSQL Flexible Server",
		provision.KindRedis:     "Azure Managed Redis",
		provision.Kind("bogus"): "Managed data service",
	}
	for kind, want := range cases {
		if got := managedResourceType(cloud.Azure, kind); got != want {
			t.Errorf("managedResourceType(azure, %q) = %q, want %q", kind, got, want)
		}
	}
	for _, kind := range []provision.Kind{provision.KindPostgres, provision.KindRedis} {
		got := managedResourceType(cloud.Azure, kind)
		for _, banned := range []string{"RDS", "ElastiCache", "Cloud SQL", "Memorystore"} {
			if strings.Contains(got, banned) {
				t.Errorf("managedResourceType(azure, %q) = %q, must not name %q", kind, got, banned)
			}
		}
	}
}

// TestRenderPlan_AzureManagedLine is the approval-gate assertion: the rendered
// line for BOTH azure provisioners names the Azure service, the per-kind Azure
// network detail and the "in resource group <rg>" scope — and none of the other
// clouds' vocabulary. The AWS line stays byte-identical.
func TestRenderPlan_AzureManagedLine(t *testing.T) {
	net := provision.AzureNetworking{
		SubscriptionID:         "sub-1",
		ResourceGroup:          "op-rg",
		PGDelegatedSubnetID:    "/subs/pg-subnet",
		PGPrivateDNSZoneID:     "/zones/pg-zone",
		RedisPrivateEndpointID: "/eps/redis-pe",
	}
	v := mkValues(t, nil)
	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "prod", Cloud: cloud.Azure}, v, &ProvisionPlan{
		Managed: []provision.Provisioner{
			provision.NewPostgresFlexibleProvisioner("prod", "eastus", net, provision.ClientConfig{}),
			provision.NewManagedRedisProvisioner("prod", "eastus", net, provision.ClientConfig{}),
		},
	})
	out := b.String()

	for _, want := range []string{
		"in resource group op-rg",
		"/subs/pg-subnet",
		"/zones/pg-zone",
		"/eps/redis-pe",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("azure plan output missing %q\n---\n%s", want, out)
		}
	}
	for _, banned := range []string{"subnet group", "security group", "in project", "RDS", "ElastiCache"} {
		if strings.Contains(out, banned) {
			t.Errorf("azure plan output contains the wrong cloud's vocabulary %q\n---\n%s", banned, out)
		}
	}

	// The AWS plan line is byte-identical to before.
	var awsOut strings.Builder
	renderPlan(&awsOut, Options{ReleaseName: "llamacloud", Namespace: "prod"}, mkValues(t, nil),
		&ProvisionPlan{Managed: []provision.Provisioner{newFakePostgresProvisioner()}})
	if got := awsOut.String(); !strings.Contains(got, "ensure managed postgres prod-llamacloud-pg (region us-east-1)\n") {
		t.Errorf("the AWS plan line changed shape\n---\n%s", got)
	}
}

// TestRunProvisioning_AzureInventoryRow drives the REAL install call site with
// cloud azure, proving runProvisioning records the row under the Azure provider
// group with the Azure service label, the RESOURCE GROUP scope noun (not
// "project") and the Azure network note.
func TestRunProvisioning_AzureInventoryRow(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	fp.identifier = "prod-llamacloud-pg"
	fp.region = "eastus"
	fp.scope = "op-rg"
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	inv := &Inventory{}
	opts := Options{
		Cloud:          cloud.Azure,
		Namespace:      "prod",
		Out:            &strings.Builder{},
		OutputPath:     filepath.Join(t.TempDir(), "v.yaml"),
		OutputExplicit: true,
		inventory:      inv,
		accountGuard:   noopAccountGuard,
	}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		t.Fatalf("runProvisioning: %v", err)
	}

	if aws := inv.byProvider("AWS"); len(aws) != 0 {
		t.Errorf("an Azure install recorded %d AWS rows, want 0: %+v", len(aws), aws)
	}
	rs := inv.byProvider("Azure")
	if len(rs) != 1 {
		t.Fatalf("expected 1 Azure resource recorded, got %d: %+v", len(rs), rs)
	}
	got := rs[0]
	if got.Type != "Azure Database for PostgreSQL Flexible Server" {
		t.Errorf("Type = %q, want the Azure product name", got.Type)
	}
	if got.Location != "eastus" {
		t.Errorf("Location = %q, want eastus", got.Location)
	}
	if !strings.Contains(got.Detail, "resource group op-rg") {
		t.Errorf("Detail = %q, want the RESOURCE GROUP scope noun", got.Detail)
	}
	if strings.Contains(got.Detail, "project op-rg") {
		t.Errorf("Detail = %q, must not call an Azure resource group a project", got.Detail)
	}
	if !strings.Contains(got.Detail, "delegated subnet") {
		t.Errorf("Detail = %q, want the Azure network note", got.Detail)
	}
	if strings.Contains(got.Detail, "subnet group") {
		t.Errorf("Detail = %q, must not carry the AWS network note", got.Detail)
	}

	// It renders under the Azure heading, and the snapshot names no other cloud's
	// service.
	var b strings.Builder
	inv.WriteText(&b)
	out := b.String()
	if !strings.Contains(out, "\nAzure\n") {
		t.Errorf("WriteText missing the Azure provider heading\n---\n%s", out)
	}
	for _, banned := range []string{"RDS", "ElastiCache", "Cloud SQL", "Memorystore", "subnet group"} {
		if strings.Contains(out, banned) {
			t.Errorf("Azure snapshot names %q\n---\n%s", banned, out)
		}
	}
}
