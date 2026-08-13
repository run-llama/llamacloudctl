package uninstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/tui"
)

// --- pure helpers ---
// (The token->Kind parser tests moved with the parser to internal/provision/kinds_test.go.)

func TestResolveManagedKinds_All(t *testing.T) {
	kinds, err := resolveManagedKinds(Options{DeleteManaged: []string{"all"}})
	if err != nil {
		t.Fatalf("resolveManagedKinds: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("\"all\" should select 2 kinds, got %d", len(kinds))
	}
}

func TestIntendedReleaseNames(t *testing.T) {
	all := intendedReleaseNames(Options{ReleaseName: "llamacloud"})
	if len(all) != 3 || all[0] != "llamacloud" {
		t.Fatalf("default set = %v, want main + 2 DBs", all)
	}
	kept := intendedReleaseNames(Options{ReleaseName: "llamacloud", KeepDatabases: true})
	if len(kept) != 1 || kept[0] != "llamacloud" {
		t.Fatalf("--keep-databases set = %v, want only the main release", kept)
	}
}

// --- fake Deprovisioner + destroyManaged ---

type fakeDeprovisioner struct {
	kind provision.Kind
	id   string
	// scope is the extra addressing context (the GCP project id). Empty on every
	// AWS-shaped fake, so the teardown line assertions match today's AWS output.
	scope      string
	destroyErr error
	destroyed  bool
}

func (f *fakeDeprovisioner) Destroy(context.Context) error { f.destroyed = true; return f.destroyErr }
func (f *fakeDeprovisioner) Kind() provision.Kind          { return f.kind }
func (f *fakeDeprovisioner) Identifier() string            { return f.id }
func (f *fakeDeprovisioner) Region() string                { return "us-east-1" }

func (f *fakeDeprovisioner) Scope() string { return f.scope }

// TestDestroyManaged_ScopeRendersProject covers the third of the three Scope render
// sites: the teardown line names the GCP project when the deprovisioner has one,
// and is byte-identical to today's AWS output when it does not.
func TestDestroyManaged_ScopeRendersProject(t *testing.T) {
	// internal/uninstall holds its OWN copy of managedScopeDetail — the two
	// packages do not import each other — so a green install-side test says
	// NOTHING about this one. Both nouns are asserted here independently.
	for _, tc := range []struct {
		c    cloud.Cloud
		want string
	}{
		{cloud.GCP, "deleting managed postgres (postgres-id) in project my-scope..."},
		{cloud.Azure, "deleting managed postgres (postgres-id) in resource group my-scope..."},
	} {
		scopedOut := &bytes.Buffer{}
		scoped := Options{
			Namespace: "prod",
			Region:    "us-central1",
			Cloud:     tc.c,
			Out:       scopedOut,
			deprovisionerFor: func(_ cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
				return &fakeDeprovisioner{kind: kind, id: string(kind) + "-id", scope: "my-scope"}, nil
			},
		}
		if err := destroyManaged(context.Background(), scoped, []provision.Kind{provision.KindPostgres}); err != nil {
			t.Fatalf("%s destroyManaged: %v", tc.c, err)
		}
		got := scopedOut.String()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s teardown line missing %q\n---\n%s", tc.c, tc.want, got)
		}
		if tc.c == cloud.Azure && strings.Contains(got, "in project") {
			t.Errorf("azure teardown line calls a resource group a project\n---\n%s", got)
		}
	}

	plainOut := &bytes.Buffer{}
	plain := Options{
		Namespace: "prod",
		Region:    "us-east-1",
		Out:       plainOut,
		deprovisionerFor: func(_ cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
			return &fakeDeprovisioner{kind: kind, id: string(kind) + "-id"}, nil
		},
	}
	if err := destroyManaged(context.Background(), plain, []provision.Kind{provision.KindPostgres}); err != nil {
		t.Fatalf("destroyManaged: %v", err)
	}
	got := plainOut.String()
	if !strings.Contains(got, "deleting managed postgres (postgres-id)...") {
		t.Errorf("unscoped teardown line changed shape\n---\n%s", got)
	}
	if strings.Contains(got, "in project") {
		t.Errorf("unscoped teardown line gained a project segment\n---\n%s", got)
	}
}

func TestDestroyManaged_AllSucceed(t *testing.T) {
	made := map[provision.Kind]*fakeDeprovisioner{}
	opts := Options{
		Namespace: "prod",
		Region:    "us-east-1",
		Out:       &bytes.Buffer{},
		deprovisionerFor: func(_ cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
			f := &fakeDeprovisioner{kind: kind, id: string(kind) + "-id"}
			made[kind] = f
			return f, nil
		},
	}
	kinds := []provision.Kind{provision.KindPostgres, provision.KindRedis}
	if err := destroyManaged(context.Background(), opts, kinds); err != nil {
		t.Fatalf("destroyManaged: %v", err)
	}
	for _, k := range kinds {
		if made[k] == nil || !made[k].destroyed {
			t.Errorf("%s was not destroyed", k)
		}
	}
}

func TestDestroyManaged_AggregatesFailures(t *testing.T) {
	opts := Options{
		Namespace: "prod",
		Out:       &bytes.Buffer{},
		deprovisionerFor: func(_ cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
			f := &fakeDeprovisioner{kind: kind, id: string(kind) + "-id"}
			if kind == provision.KindRedis {
				f.destroyErr = errors.New("boom")
			}
			return f, nil
		},
	}
	err := destroyManaged(context.Background(), opts, []provision.Kind{provision.KindPostgres, provision.KindRedis})
	if err == nil || !strings.Contains(err.Error(), "redis") {
		t.Fatalf("expected an aggregated failure naming redis, got %v", err)
	}
}

// --- Run gating / planning ---

func TestRun_NonInteractiveWithoutYesFails(t *testing.T) {
	err := Run(context.Background(), Options{
		Namespace:      "prod",
		NonInteractive: true,
		Out:            &bytes.Buffer{},
		SkipCluster:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires confirmation") {
		t.Fatalf("want a confirmation error, got %v", err)
	}
}

func TestRun_YesSkipClusterRendersPlan(t *testing.T) {
	var buf bytes.Buffer
	err := Run(context.Background(), Options{
		Namespace:     "prod",
		Yes:           true,
		DeleteManaged: []string{"postgres"},
		Out:           &buf,
		SkipCluster:   true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Uninstall plan for namespace \"prod\"") {
		t.Errorf("plan header missing:\n%s", out)
	}
	if !strings.Contains(out, "Managed cloud data services to DELETE") || !strings.Contains(out, "postgres") {
		t.Errorf("managed delete line missing:\n%s", out)
	}
}

func TestRun_InteractiveDeclineAborts(t *testing.T) {
	// The interactive path prompts three times; answer the managed multi-select and
	// the namespace confirm, then DECLINE the final confirmation.
	p := tui.NewNonInteractive(map[string]any{
		"Also DELETE managed cloud data services? (destructive — skips final snapshots; leave empty to keep them)": []string{},
		"Also delete the namespace \"prod\" itself (removes PVCs, Secrets, everything left behind)?":               false,
		"Uninstall from namespace \"prod\" now? This cannot be undone.":                                            false,
	})
	err := Run(context.Background(), Options{
		Namespace:   "prod",
		Out:         &bytes.Buffer{},
		Prompter:    p,
		SkipCluster: true,
	})
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("want a declined error, got %v", err)
	}
}

func TestRun_InteractiveSelectsManagedAndNamespace(t *testing.T) {
	var buf bytes.Buffer
	p := tui.NewNonInteractive(map[string]any{
		"Also DELETE managed cloud data services? (destructive — skips final snapshots; leave empty to keep them)": []string{"redis"},
		"Also delete the namespace \"prod\" itself (removes PVCs, Secrets, everything left behind)?":               true,
		"Uninstall from namespace \"prod\" now? This cannot be undone.":                                            true,
	})
	err := Run(context.Background(), Options{
		Namespace:   "prod",
		Out:         &buf,
		Prompter:    p,
		SkipCluster: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "redis") {
		t.Errorf("expected redis in the managed-delete plan:\n%s", out)
	}
	if !strings.Contains(out, "DELETE (removes PVCs") {
		t.Errorf("expected the namespace delete line:\n%s", out)
	}
}

// TestDestroyManaged_RoutesByCloud proves --cloud gcp routes to the GCP
// deprovisioners with the project scope and --cloud azure to the Azure ones with
// the subscription + resource group, while the default (no --cloud) still routes
// to the AWS ones. Before the factory took a cloud it built RDS/ElastiCache
// unconditionally, so a GCP or Azure teardown would have issued AWS calls.
func TestDestroyManaged_RoutesByCloud(t *testing.T) {
	cases := []struct {
		name      string
		cloudID   cloud.Cloud
		scope     provision.Scope
		wantTypes map[provision.Kind]string
	}{
		{"default is aws", cloud.Cloud(""), provision.Scope{}, map[provision.Kind]string{
			provision.KindPostgres: "*provision.RDSProvisioner",
			provision.KindRedis:    "*provision.ElastiCacheProvisioner",
		}},
		{"gcp", cloud.GCP, provision.Scope{Project: "op-project"}, map[provision.Kind]string{
			provision.KindPostgres: "*provision.CloudSQLProvisioner",
			provision.KindRedis:    "*provision.MemorystoreProvisioner",
		}},
		{"azure", cloud.Azure, provision.Scope{SubscriptionID: "sub-1", ResourceGroup: "op-rg"}, map[provision.Kind]string{
			provision.KindPostgres: "*provision.PostgresFlexibleProvisioner",
			provision.KindRedis:    "*provision.ManagedRedisProvisioner",
		}},
	}
	for _, tc := range cases {
		got := map[provision.Kind]string{}
		var sawScope provision.Scope
		opts := Options{
			Namespace:           "prod",
			Region:              "us-central1",
			Cloud:               tc.cloudID,
			GCPProject:          tc.scope.Project,
			AzureSubscriptionID: tc.scope.SubscriptionID,
			AzureResourceGroup:  tc.scope.ResourceGroup,
			Out:                 &bytes.Buffer{},
			// The seam forwards to the REAL factory, so this exercises the actual
			// per-cloud dispatch rather than a stand-in.
			deprovisionerFor: func(c cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
				sawScope = scope
				dp, err := provision.NewDeprovisioner(c, kind, ns, region, scope, cc)
				if err != nil {
					return nil, err
				}
				got[kind] = fmt.Sprintf("%T", dp)
				// Swap in a fake so Destroy touches no cloud.
				return &fakeDeprovisioner{kind: kind, id: dp.Identifier(), scope: dp.Scope()}, nil
			},
		}
		kinds := []provision.Kind{provision.KindPostgres, provision.KindRedis}
		if err := destroyManaged(context.Background(), opts, kinds); err != nil {
			t.Fatalf("%s: destroyManaged: %v", tc.name, err)
		}
		for _, kind := range kinds {
			if got[kind] != tc.wantTypes[kind] {
				t.Errorf("%s/%s: built %s, want %s", tc.name, kind, got[kind], tc.wantTypes[kind])
			}
		}
		if sawScope != tc.scope {
			t.Errorf("%s: scope passed to the factory = %+v, want %+v", tc.name, sawScope, tc.scope)
		}
	}
}

// TestTeardownCloudLabel proves the async-teardown line names the right cloud
// rather than hard-coding AWS.
// TestDestroyManaged_NonAWSRefusesUnsetRegion is the teardown twin of the create
// side's refusal. destroyManaged used to pass opts.Region through VERBATIM with
// no fallback at all, so an azure/gcp `uninstall --delete-managed` with no
// --region handed an EMPTY region to the deprovisioner. It must now abort BEFORE
// any deprovisioner is constructed.
//
// The AWS path is unchanged: it has always accepted an empty region, and must
// keep doing so.
func TestDestroyManaged_NonAWSRefusesUnsetRegion(t *testing.T) {
	for _, c := range []cloud.Cloud{cloud.Azure, cloud.GCP} {
		t.Run(string(c), func(t *testing.T) {
			var built int
			opts := Options{
				Namespace: "prod",
				Cloud:     c,
				// Region deliberately unset.
				Out: &bytes.Buffer{},
				deprovisionerFor: func(_ cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
					built++
					return &fakeDeprovisioner{kind: kind, id: string(kind) + "-id"}, nil
				},
			}
			err := destroyManaged(context.Background(), opts, []provision.Kind{provision.KindPostgres})
			if err == nil {
				t.Fatalf("managed teardown on %q with no --region must abort, got nil", c)
			}
			if !strings.Contains(err.Error(), "--region") {
				t.Errorf("error = %q, want it to name the --region flag", err)
			}
			if built != 0 {
				t.Errorf("built %d deprovisioner(s); the refusal must land before any is constructed", built)
			}
		})
	}

	// AWS is untouched: an empty region still runs, because the AWS
	// deprovisioners resolve one from the environment themselves.
	for _, c := range []cloud.Cloud{cloud.AWS, cloud.Cloud("")} {
		t.Run("aws-unchanged/"+string(c), func(t *testing.T) {
			opts := Options{
				Namespace: "prod",
				Cloud:     c,
				Out:       &bytes.Buffer{},
				deprovisionerFor: func(_ cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
					return &fakeDeprovisioner{kind: kind, id: string(kind) + "-id"}, nil
				},
			}
			if err := destroyManaged(context.Background(), opts, []provision.Kind{provision.KindPostgres}); err != nil {
				t.Fatalf("AWS teardown with an empty region must still run, got %v", err)
			}
		})
	}
}

func TestTeardownCloudLabel(t *testing.T) {
	cases := map[cloud.Cloud]string{
		cloud.Cloud(""): "AWS",
		cloud.AWS:       "AWS",
		cloud.GCP:       "GCP",
		cloud.Azure:     "Azure",
	}
	for c, want := range cases {
		if got := teardownCloudLabel(c); got != want {
			t.Errorf("teardownCloudLabel(%q) = %q, want %q", c, got, want)
		}
	}

	out := &bytes.Buffer{}
	opts := Options{
		Namespace: "prod",
		Cloud:     cloud.GCP,
		// Managed teardown off AWS REQUIRES an explicit region:
		// destroyManaged used to pass an unset one straight through, so an
		// azure/gcp teardown addressed the resource with an empty region.
		Region: "us-central1",
		Out:    out,
		deprovisionerFor: func(_ cloud.Cloud, kind provision.Kind, ns, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error) {
			return &fakeDeprovisioner{kind: kind, id: string(kind) + "-id"}, nil
		},
	}
	if err := destroyManaged(context.Background(), opts, []provision.Kind{provision.KindPostgres}); err != nil {
		t.Fatalf("destroyManaged: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "(GCP tears it down asynchronously)") {
		t.Errorf("teardown line must name GCP, got:\n%s", got)
	}
}
