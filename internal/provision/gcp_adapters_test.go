package provision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/api/option"
	redis "google.golang.org/api/redis/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// The GCP adapters in gcp.go drive the generated client builder chains. These
// tests point those clients at a LOCAL httptest server with a no-auth
// credentials option, so the adapter behaviour is exercised with no GCP project
// and no outbound request.

// gcpStub is a local API server that records the requests it saw and answers
// each with a canned status and body.
type gcpStub struct {
	srv     *httptest.Server
	status  int
	body    string
	paths   []string
	method  []string
	queries []url.Values
}

func newGCPStub(t *testing.T, status int, body string) *gcpStub {
	t.Helper()
	stub := &gcpStub{status: status, body: body}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.paths = append(stub.paths, r.URL.Path)
		stub.method = append(stub.method, r.Method)
		stub.queries = append(stub.queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.status)
		_, _ = w.Write([]byte(stub.body))
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (g *gcpStub) sqlAdapter(t *testing.T) sqladminClient {
	t.Helper()
	svc, err := sqladmin.NewService(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(g.srv.URL),
		option.WithHTTPClient(g.srv.Client()))
	if err != nil {
		t.Fatalf("build Cloud SQL client: %v", err)
	}
	return sqladminClient{svc: svc}
}

func (g *gcpStub) redisAdapter(t *testing.T) memorystoreClient {
	t.Helper()
	svc, err := redis.NewService(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(g.srv.URL),
		option.WithHTTPClient(g.srv.Client()))
	if err != nil {
		t.Fatalf("build Memorystore client: %v", err)
	}
	return memorystoreClient{svc: svc}
}

// TestCloudSQLGetInstance proves the read adapter addresses project+instance and
// decodes the response.
func TestCloudSQLGetInstance(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"name":"llamacloud-postgres","region":"us-central1","state":"RUNNABLE"}`)

	inst, err := stub.sqlAdapter(t).GetInstance(context.Background(), "proj-1", "llamacloud-postgres")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Name != "llamacloud-postgres" {
		t.Errorf("instance name = %q, want llamacloud-postgres", inst.Name)
	}
	if inst.State != "RUNNABLE" {
		t.Errorf("state = %q, want RUNNABLE", inst.State)
	}
	if len(stub.paths) != 1 || !strings.Contains(stub.paths[0], "/projects/proj-1/instances/llamacloud-postgres") {
		t.Errorf("request paths = %v, want the project and instance addressed", stub.paths)
	}
}

// TestCloudSQLGetInstanceErrorPropagates proves a 404 is returned as an error —
// unlike Azure, the Cloud SQL adapter does NOT swallow not-found, so the
// provisioner's own create-or-adopt logic classifies it.
func TestCloudSQLGetInstanceErrorPropagates(t *testing.T) {
	stub := newGCPStub(t, http.StatusNotFound, `{"error":{"code":404,"message":"instance does not exist"}}`)

	_, err := stub.sqlAdapter(t).GetInstance(context.Background(), "proj-1", "missing")
	if err == nil {
		t.Fatal("a 404 must be returned as an error by this adapter")
	}
	if !strings.Contains(err.Error(), "instance does not exist") {
		t.Errorf("error = %q, want the API message preserved", err)
	}
}

// TestCloudSQLInsertInstance proves the create adapter POSTs to the project's
// instances collection.
func TestCloudSQLInsertInstance(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"name":"op-1","status":"DONE"}`)

	err := stub.sqlAdapter(t).InsertInstance(context.Background(), "proj-1",
		&sqladmin.DatabaseInstance{Name: "llamacloud-postgres", Region: "us-central1"})
	if err != nil {
		t.Fatalf("InsertInstance: %v", err)
	}
	if stub.method[0] != http.MethodPost {
		t.Errorf("method = %s, want POST", stub.method[0])
	}
	if !strings.HasSuffix(stub.paths[0], "/projects/proj-1/instances") {
		t.Errorf("path = %q, want the project's instances collection", stub.paths[0])
	}
}

// TestCloudSQLPatchInstance proves the update adapter PATCHes the named
// instance.
func TestCloudSQLPatchInstance(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"name":"op-1","status":"DONE"}`)

	err := stub.sqlAdapter(t).PatchInstance(context.Background(), "proj-1", "llamacloud-postgres",
		&sqladmin.DatabaseInstance{Name: "llamacloud-postgres"})
	if err != nil {
		t.Fatalf("PatchInstance: %v", err)
	}
	if stub.method[0] != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", stub.method[0])
	}
	if !strings.HasSuffix(stub.paths[0], "/instances/llamacloud-postgres") {
		t.Errorf("path = %q, want the named instance", stub.paths[0])
	}
}

// TestCloudSQLDeleteInstance proves the teardown adapter DELETEs the named
// instance.
func TestCloudSQLDeleteInstance(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"name":"op-1","status":"DONE"}`)

	if err := stub.sqlAdapter(t).DeleteInstance(context.Background(), "proj-1", "llamacloud-postgres"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if stub.method[0] != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", stub.method[0])
	}
}

// TestCloudSQLInsertDatabase proves the logical-database adapter posts under the
// instance's databases collection.
func TestCloudSQLInsertDatabase(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"name":"op-1","status":"DONE"}`)

	err := stub.sqlAdapter(t).InsertDatabase(context.Background(), "proj-1", "llamacloud-postgres",
		&sqladmin.Database{Name: "llamacloud"})
	if err != nil {
		t.Fatalf("InsertDatabase: %v", err)
	}
	if !strings.HasSuffix(stub.paths[0], "/instances/llamacloud-postgres/databases") {
		t.Errorf("path = %q, want the instance's databases collection", stub.paths[0])
	}
}

// TestCloudSQLInsertDatabaseErrorPropagates proves a rejected database create
// surfaces rather than being reported as success.
func TestCloudSQLInsertDatabaseErrorPropagates(t *testing.T) {
	stub := newGCPStub(t, http.StatusConflict, `{"error":{"code":409,"message":"database already exists"}}`)

	err := stub.sqlAdapter(t).InsertDatabase(context.Background(), "proj-1", "pg", &sqladmin.Database{Name: "llamacloud"})
	if err == nil {
		t.Fatal("a rejected database create must return an error for the caller to classify")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want the API message preserved", err)
	}
}

// TestMemorystoreGetInstance proves the read adapter addresses the fully
// qualified instance name and decodes the response.
func TestMemorystoreGetInstance(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK,
		`{"name":"projects/proj-1/locations/us-central1/instances/llamacloud-redis","state":"READY","host":"10.0.0.3","port":6379}`)

	inst, err := stub.redisAdapter(t).GetInstance(context.Background(), "projects/proj-1/locations/us-central1/instances/llamacloud-redis")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.State != "READY" {
		t.Errorf("state = %q, want READY", inst.State)
	}
	if inst.Host != "10.0.0.3" || inst.Port != 6379 {
		t.Errorf("endpoint = %s:%d, want 10.0.0.3:6379", inst.Host, inst.Port)
	}
}

// TestMemorystoreCreateInstance proves the create adapter posts to the parent
// collection and passes the instance id as a query parameter — Memorystore names
// the resource from that parameter, not from the body.
func TestMemorystoreCreateInstance(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"name":"op-1","done":true}`)

	err := stub.redisAdapter(t).CreateInstance(context.Background(),
		"projects/proj-1/locations/us-central1", "llamacloud-redis", &redis.Instance{Tier: "STANDARD_HA"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if len(stub.paths) != 1 {
		t.Fatalf("sent %d requests, want 1", len(stub.paths))
	}
	if !strings.HasSuffix(stub.paths[0], "/locations/us-central1/instances") {
		t.Errorf("path = %q, want the parent's instances collection", stub.paths[0])
	}
	if got := stub.queries[0].Get("instanceId"); got != "llamacloud-redis" {
		t.Errorf("instanceId = %q, want llamacloud-redis", got)
	}
}

// TestMemorystoreDeleteInstance proves the teardown adapter DELETEs the named
// instance.
func TestMemorystoreDeleteInstance(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"name":"op-1","done":true}`)

	err := stub.redisAdapter(t).DeleteInstance(context.Background(),
		"projects/proj-1/locations/us-central1/instances/llamacloud-redis")
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if stub.method[0] != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", stub.method[0])
	}
}

// TestMemorystoreGetAuthString proves the SERVICE-minted AUTH string is read
// back — the provisioner never generates one locally, so this value is the
// chart's Redis password.
func TestMemorystoreGetAuthString(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{"authString":"service-minted-secret"}`)

	got, err := stub.redisAdapter(t).GetAuthString(context.Background(),
		"projects/proj-1/locations/us-central1/instances/llamacloud-redis")
	if err != nil {
		t.Fatalf("GetAuthString: %v", err)
	}
	if got != "service-minted-secret" {
		t.Errorf("auth string = %q, want service-minted-secret", got)
	}
	if !strings.HasSuffix(stub.paths[0], "/instances/llamacloud-redis/authString") {
		t.Errorf("path = %q, want the instance's authString subresource", stub.paths[0])
	}
}

// TestMemorystoreGetAuthStringEmpty proves an instance with AUTH disabled yields
// an empty string rather than an error — the caller decides what that means.
func TestMemorystoreGetAuthStringEmpty(t *testing.T) {
	stub := newGCPStub(t, http.StatusOK, `{}`)

	got, err := stub.redisAdapter(t).GetAuthString(context.Background(), "projects/p/locations/l/instances/i")
	if err != nil {
		t.Fatalf("GetAuthString: %v", err)
	}
	if got != "" {
		t.Errorf("auth string = %q, want empty", got)
	}
}

// TestMemorystoreGetAuthStringErrorPropagates proves a failed read surfaces
// instead of handing back an empty credential the chart would then use.
func TestMemorystoreGetAuthStringErrorPropagates(t *testing.T) {
	stub := newGCPStub(t, http.StatusForbidden, `{"error":{"code":403,"message":"permission denied"}}`)

	got, err := stub.redisAdapter(t).GetAuthString(context.Background(), "projects/p/locations/l/instances/i")
	if err == nil {
		t.Fatal("a failed auth-string read must return an error, not an empty credential")
	}
	if got != "" {
		t.Errorf("auth string = %q, want empty alongside the error", got)
	}
}
