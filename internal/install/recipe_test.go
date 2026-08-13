package install

import "testing"

// TestInClusterDBReleases pins the recipe: two releases with the exact charts +
// pinned versions from the proven in-cluster recipe.
func TestInClusterDBReleases(t *testing.T) {
	want := map[string]struct{ chart, version string }{
		"postgresql": {"oci://registry-1.docker.io/bitnamicharts/postgresql", "18.7.3"},
		"redis":      {"oci://registry-1.docker.io/bitnamicharts/redis", "27.0.4"},
	}
	releases := inClusterDBReleases("")
	if len(releases) != len(want) {
		t.Fatalf("inClusterDBReleases = %d releases, want %d", len(releases), len(want))
	}
	for _, r := range releases {
		w, ok := want[r.Release]
		if !ok {
			t.Errorf("unexpected release %q", r.Release)
			continue
		}
		if r.Chart != w.chart {
			t.Errorf("%s chart = %q, want %q", r.Release, r.Chart, w.chart)
		}
		if r.Version != w.version {
			t.Errorf("%s version = %q, want %q", r.Release, r.Version, w.version)
		}
		// Every bitnamilegacy/xavidop image needs allowInsecureImages.
		g, _ := r.Values["global"].(map[string]any)
		sec, _ := g["security"].(map[string]any)
		if sec["allowInsecureImages"] != true {
			t.Errorf("%s recipe missing global.security.allowInsecureImages", r.Release)
		}
	}
}

// TestWireInClusterDBValues pins the deterministic endpoints + temporal.deploy.
func TestWireInClusterDBValues(t *testing.T) {
	v := mkValues(t, nil)
	wireInClusterDBValues(v)
	wantStrings := map[string]string{
		"postgresql.host":     inClusterPGHost,
		"postgresql.port":     inClusterPGPort,
		"postgresql.database": inClusterPGDatabase,
		"redis.host":          inClusterRedisHost,
		"redis.scheme":        "redis",
	}
	for k, want := range wantStrings {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// redis.db is an integer; temporal deploys in-cluster.
	if raw, _ := v.Get("redis.db"); raw != 0 {
		t.Errorf("redis.db = %v (%T), want int 0", raw, raw)
	}
	if b, _ := v.Get("temporal.deploy"); b != true {
		t.Errorf("temporal.deploy = %v, want true", b)
	}
	if b, _ := v.Get("temporal.disabled"); b != false {
		t.Errorf("temporal.disabled = %v, want false", b)
	}
	// The removed data services are explicitly disabled so the chart does not
	// require their endpoints.
	for _, svc := range []string{"mongodb", "rabbitmq", "qdrant"} {
		if b, _ := v.Get(svc + ".enabled"); b != false {
			t.Errorf("%s.enabled = %v, want false", svc, b)
		}
	}
	// ...and the v1 parse worker, which consumes only the rabbitmq queue just
	// disabled, is scaled to zero so the rollout wait can converge.
	if n, _ := v.Get("llamaParse.replicas"); n != 0 {
		t.Errorf("llamaParse.replicas = %v, want 0 on the in-cluster path", n)
	}
}

// TestDBReleaseFor_UnknownHasNoRecipe confirms a service with no in-cluster recipe
// resolves to no release, so the wizard rejects in-cluster for it, while the
// postgres alias resolves to the postgresql recipe.
func TestDBReleaseFor_UnknownHasNoRecipe(t *testing.T) {
	if _, ok := dbReleaseFor("qdrant", ""); ok {
		t.Error("qdrant should have no in-cluster recipe")
	}
	if _, ok := dbReleaseFor("postgres", ""); !ok {
		t.Error("postgres (alias) should resolve to the postgresql recipe")
	}
}

// TestInClusterDBReleases_StorageClassInjected proves a non-empty storageClass is
// injected into global.storageClass on EVERY release (so the DB PVCs bind on
// clusters with no default StorageClass), while allowInsecureImages is preserved.
func TestInClusterDBReleases_StorageClassInjected(t *testing.T) {
	releases := inClusterDBReleases("gp2")
	if len(releases) != 2 {
		t.Fatalf("inClusterDBReleases = %d releases, want 2", len(releases))
	}
	for _, r := range releases {
		g, _ := r.Values["global"].(map[string]any)
		if g["storageClass"] != "gp2" {
			t.Errorf("%s global.storageClass = %v, want gp2", r.Release, g["storageClass"])
		}
		// allowInsecureImages must survive alongside storageClass.
		sec, _ := g["security"].(map[string]any)
		if sec["allowInsecureImages"] != true {
			t.Errorf("%s lost global.security.allowInsecureImages when storageClass set", r.Release)
		}
	}
}

// TestInClusterDBReleases_NoStorageClassByDefault proves an empty storageClass
// injects NO global.storageClass key (the cluster default is used).
func TestInClusterDBReleases_NoStorageClassByDefault(t *testing.T) {
	for _, r := range inClusterDBReleases("") {
		g, _ := r.Values["global"].(map[string]any)
		if _, ok := g["storageClass"]; ok {
			t.Errorf("%s injected global.storageClass with empty storageClass (should use cluster default): %v", r.Release, g["storageClass"])
		}
	}
}

// TestInClusterDBReleases_PostgresExplicitResources proves postgres carries
// explicit primary.resources (requests+limits) and NOT resourcesPreset "micro"
// (the preset OOMKills it), while redis keeps the "micro" preset.
func TestInClusterDBReleases_PostgresExplicitResources(t *testing.T) {
	byName := map[string]DBRelease{}
	for _, r := range inClusterDBReleases("") {
		byName[r.Release] = r
	}

	pg := byName["postgresql"]
	if _, ok := pg.Values["resourcesPreset"]; ok {
		t.Errorf("postgres still carries resourcesPreset (the preset OOMKills it); want explicit resources")
	}
	primary, ok := pg.Values["primary"].(map[string]any)
	if !ok {
		t.Fatalf("postgres missing primary block; want primary.resources")
	}
	res, ok := primary["resources"].(map[string]any)
	if !ok {
		t.Fatalf("postgres missing primary.resources")
	}
	reqs, _ := res["requests"].(map[string]any)
	lims, _ := res["limits"].(map[string]any)
	if reqs["cpu"] != "250m" || reqs["memory"] != "512Mi" {
		t.Errorf("postgres requests = %v, want cpu=250m memory=512Mi", reqs)
	}
	if lims["cpu"] != "1" || lims["memory"] != "1Gi" {
		t.Errorf("postgres limits = %v, want cpu=1 memory=1Gi", lims)
	}

	// redis keeps the "micro" preset and carries NO explicit resources.
	for _, name := range []string{"redis"} {
		r := byName[name]
		if r.Values["resourcesPreset"] != "micro" {
			t.Errorf("%s resourcesPreset = %v, want micro", name, r.Values["resourcesPreset"])
		}
		if _, ok := r.Values["primary"]; ok {
			t.Errorf("%s should not carry a primary.resources block (only postgres does)", name)
		}
	}
}

// TestInClusterDBReleases_KarpenterDoNotDisrupt proves the stateful data-service
// pods carry the karpenter.sh/do-not-disrupt annotation. Without it, Karpenter
// consolidation evicts these PVC-backed singletons mid-install, bouncing the EBS
// volume (multi-attach delay + WAL recovery) and crash-looping the Temporal
// history service until helm --wait times out ("context deadline exceeded").
func TestInClusterDBReleases_KarpenterDoNotDisrupt(t *testing.T) {
	byName := map[string]DBRelease{}
	for _, r := range inClusterDBReleases("") {
		byName[r.Release] = r
	}

	// postgres: primary.podAnnotations
	pgPrimary, _ := byName["postgresql"].Values["primary"].(map[string]any)
	assertDoNotDisrupt(t, "postgresql primary", pgPrimary["podAnnotations"])

	// redis (standalone): master.podAnnotations
	redisMaster, _ := byName["redis"].Values["master"].(map[string]any)
	assertDoNotDisrupt(t, "redis master", redisMaster["podAnnotations"])
}

// TestWireInClusterTemporal_KarpenterDoNotDisrupt proves the Temporal server pods
// (frontend/history/matching/worker) are pinned off Karpenter consolidation via
// temporal-subchart.server.podAnnotations, which the chart merges onto every
// server pod.
func TestWireInClusterTemporal_KarpenterDoNotDisrupt(t *testing.T) {
	v := mkValues(t, nil)
	wireInClusterTemporal(v, inClusterPGHost, inClusterPGPort, inClusterPGUser, inClusterPGPassword)
	got, _ := v.Get("temporal-subchart.server.podAnnotations")
	assertDoNotDisrupt(t, "temporal-subchart.server", got)
}

func assertDoNotDisrupt(t *testing.T, where string, ann any) {
	t.Helper()
	m, ok := ann.(map[string]any)
	if !ok {
		t.Fatalf("%s podAnnotations = %v (%T), want map with karpenter.sh/do-not-disrupt", where, ann, ann)
	}
	if m["karpenter.sh/do-not-disrupt"] != "true" {
		t.Errorf("%s karpenter.sh/do-not-disrupt = %v, want \"true\"", where, m["karpenter.sh/do-not-disrupt"])
	}
}
