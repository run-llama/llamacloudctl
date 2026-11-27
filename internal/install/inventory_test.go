package install

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/provision"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
)

// sampleManifest is a rendered Helm manifest with the mix the snapshot must handle:
// named workloads (Deployment, Service), and noisy plumbing (ConfigMap, Secret) that
// the text form collapses to counts. Leading separators / a comment-only doc exercise
// the skip-empty path.
const sampleManifest = `
---
# Source: llamacloud/templates/backend.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend
  namespace: prod
---
apiVersion: v1
kind: Service
metadata:
  name: backend
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-a
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg-b
---
apiVersion: v1
kind: Secret
metadata:
  name: creds
---
# a comment-only document with no kind
`

// TestParseManifestObjects_SkipsEmptyAndSorts checks the manifest split extracts
// kind/name/namespace, skips documents with no kind+name, and sorts by (kind, name).
func TestParseManifestObjects_SkipsEmptyAndSorts(t *testing.T) {
	objs := parseManifestObjects(sampleManifest)
	got := make([]string, 0, len(objs))
	for _, o := range objs {
		got = append(got, o.kind+"/"+o.name)
	}
	want := []string{"ConfigMap/cfg-a", "ConfigMap/cfg-b", "Deployment/backend", "Secret/creds", "Service/backend"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("parseManifestObjects = %v, want %v", got, want)
	}
	// The Deployment carries its own namespace; the Service does not.
	for _, o := range objs {
		if o.kind == "Deployment" && o.namespace != "prod" {
			t.Errorf("Deployment namespace = %q, want prod", o.namespace)
		}
		if o.kind == "Service" && o.namespace != "" {
			t.Errorf("Service namespace = %q, want empty (inherits release ns)", o.namespace)
		}
	}
}

// TestInventory_NilSafe verifies every method tolerates a nil ledger — the
// non-recording path (tests driving runProvisioning without a live Run).
func TestInventory_NilSafe(t *testing.T) {
	var inv *Inventory
	inv.Add(Resource{Provider: "AWS", Type: "S3 bucket", Name: "b"})
	inv.addManagedResource("", "RDS PostgreSQL", "pg", "us-east-1", "endpoint h:5432", "", "", "note")
	inv.addRelease(&release.Release{Name: "x"}, "prod", false)
	if !inv.Empty() {
		t.Error("nil inventory should stay empty")
	}
	out := &strings.Builder{}
	inv.WriteText(out) // must not panic
	if err := inv.WriteJSON(out); err != nil {
		t.Errorf("WriteJSON on nil inventory: %v", err)
	}
}

// TestInventory_WriteText_GroupsAndCollapses builds a full snapshot (AWS buckets +
// managed DB, and a Helm release with named + plumbing objects) and asserts the text
// form groups by provider, lists releases and named workloads individually, and
// collapses the plumbing kinds to per-kind counts.
func TestInventory_WriteText_GroupsAndCollapses(t *testing.T) {
	inv := &Inventory{}
	inv.Add(Resource{Provider: "AWS", Type: "S3 bucket", Name: "prod-storage", Location: "us-west-2"})
	// An EMPTY provider must default to "AWS" and an empty scope must contribute no
	// detail segment, so this row is byte-identical to what the pre-multi-cloud
	// signature produced.
	inv.addManagedResource("", "RDS PostgreSQL", "prod-llamacloud-pg", "us-east-1", "endpoint pg.rds:5432", "", "", "placed in the operator-supplied subnet group + security group(s)")
	inv.addRelease(&release.Release{
		Name:      "llamacloud",
		Namespace: "prod",
		Version:   1,
		Info:      &release.Info{Status: rcommon.StatusDeployed},
		Chart:     &chart.Chart{Metadata: &chart.Metadata{Name: "llamacloud", Version: "1.2.3"}},
		Manifest:  sampleManifest,
	}, "prod", false)

	out := &strings.Builder{}
	inv.WriteText(out)
	got := out.String()

	for _, want := range []string{
		"Resource summary",
		"\nAWS\n",
		"S3 bucket", "prod-storage", "us-west-2",
		"RDS PostgreSQL", "prod-llamacloud-pg", "endpoint pg.rds:5432", "placed in the operator-supplied subnet group + security group(s)",
		"Kubernetes (namespace prod)",
		"Helm release", "chart llamacloud-1.2.3", "status deployed, revision 1",
		"Deployment", "backend",
		"Service",
		"supporting objects:", "2 x ConfigMap", "1 x Secret",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("WriteText output missing %q:\n%s", want, got)
		}
	}
	// The collapsed plumbing kinds must NOT appear as individual named rows.
	if strings.Contains(got, "cfg-a") || strings.Contains(got, "creds") {
		t.Errorf("plumbing objects should be collapsed, not named:\n%s", got)
	}
	// AWS must render before Kubernetes.
	if strings.Index(got, "\nAWS\n") > strings.Index(got, "Kubernetes (namespace prod)") {
		t.Errorf("AWS group should precede Kubernetes group:\n%s", got)
	}
}

// TestInventory_WriteJSON_FullList asserts the JSON form is the COMPLETE, un-collapsed
// inventory (every ConfigMap present), unlike the text form.
func TestInventory_WriteJSON_FullList(t *testing.T) {
	inv := &Inventory{}
	inv.Add(Resource{Provider: "AWS", Type: "S3 bucket", Name: "b", Location: "us-east-1"})
	inv.addRelease(&release.Release{Name: "r", Namespace: "prod", Manifest: sampleManifest}, "prod", false)

	out := &strings.Builder{}
	if err := inv.WriteJSON(out); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var payload struct {
		Resources []Resource `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("unmarshal JSON snapshot: %v\n%s", err, out.String())
	}
	// 1 bucket + 1 release + 5 manifest objects (2 ConfigMap, 1 Secret, 1 Deployment, 1 Service).
	if len(payload.Resources) != 7 {
		t.Errorf("JSON resources = %d, want 7 (nothing collapsed):\n%s", len(payload.Resources), out.String())
	}
	var configMaps int
	for _, r := range payload.Resources {
		if r.Type == "ConfigMap" {
			configMaps++
		}
	}
	if configMaps != 2 {
		t.Errorf("JSON should list both ConfigMaps individually, got %d", configMaps)
	}
}

// TestRunProvisioning_RecordsManagedResource drives runProvisioning with a live
// inventory and a fake managed provisioner, asserting the ensured resource lands in
// the ledger with its endpoint and network note — the record site that feeds the
// end-of-run snapshot.
func TestRunProvisioning_RecordsManagedResource(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	inv := &Inventory{}
	opts := Options{
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

	aws := inv.byProvider("AWS")
	if len(aws) != 1 {
		t.Fatalf("expected 1 AWS resource recorded, got %d: %+v", len(aws), aws)
	}
	got := aws[0]
	if got.Type != "RDS PostgreSQL" || got.Name != "prod-llamacloud-pg" || got.Location != "us-east-1" {
		t.Errorf("recorded managed resource = %+v, want RDS PostgreSQL/prod-llamacloud-pg/us-east-1", got)
	}
	if !strings.Contains(got.Detail, "endpoint pg.managed.rds.amazonaws.com:5432") {
		t.Errorf("managed resource detail missing endpoint: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "subnet group") {
		t.Errorf("managed resource detail missing network note: %q", got.Detail)
	}
}
