package provision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	armpg "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v5"
	armredis "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/redisenterprise/armredisenterprise/v3"
)

// The Azure SDK adapters in azure.go wrap the real ARM clients. These tests
// build those clients over an IN-PROCESS transport that answers every request
// from a canned script, so the adapter logic (not-found mapping, poller
// collapsing, key extraction) is exercised with no Azure account and no socket.

// staticCred is a TokenCredential that mints a fake bearer token without
// contacting any identity endpoint.
type staticCred struct{}

func (staticCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// scriptedTransport answers every ARM request with a fixed status and body, and
// records the requests it saw so a test can assert what the adapter sent.
type scriptedTransport struct {
	status int
	body   string
	err    error
	seen   []*http.Request
}

func (s *scriptedTransport) Do(req *http.Request) (*http.Response, error) {
	s.seen = append(s.seen, req)
	if s.err != nil {
		return nil, s.err
	}
	body := s.body
	if body == "" {
		body = "{}"
	}
	return &http.Response{
		StatusCode: s.status,
		Status:     http.StatusText(s.status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

// armOptions wires the scripted transport into an ARM client, and disables retry
// so an error-path test does not sit through the SDK's backoff schedule.
func armOptions(tr *scriptedTransport) *arm.ClientOptions {
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: tr}}
	opts.Retry.MaxRetries = -1
	return opts
}

func newPGAdapter(t *testing.T, tr *scriptedTransport) azurePGClient {
	t.Helper()
	servers, err := armpg.NewServersClient("sub-1", staticCred{}, armOptions(tr))
	if err != nil {
		t.Fatalf("servers client: %v", err)
	}
	databases, err := armpg.NewDatabasesClient("sub-1", staticCred{}, armOptions(tr))
	if err != nil {
		t.Fatalf("databases client: %v", err)
	}
	return azurePGClient{servers: servers, databases: databases}
}

func newRedisAdapter(t *testing.T, tr *scriptedTransport) azureRedisClient {
	t.Helper()
	clusters, err := armredis.NewClient("sub-1", staticCred{}, armOptions(tr))
	if err != nil {
		t.Fatalf("clusters client: %v", err)
	}
	databases, err := armredis.NewDatabasesClient("sub-1", staticCred{}, armOptions(tr))
	if err != nil {
		t.Fatalf("databases client: %v", err)
	}
	return azureRedisClient{clusters: clusters, databases: databases}
}

// TestAzurePGGetServerFound proves an existing Flexible Server is returned with
// found=true and its fields decoded.
func TestAzurePGGetServerFound(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"name":"llamacloud-postgres","location":"eastus"}`}

	srv, found, err := newPGAdapter(t, tr).GetServer(context.Background(), "rg-1", "llamacloud-postgres")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true for a 200 response")
	}
	if srv.Name == nil || *srv.Name != "llamacloud-postgres" {
		t.Errorf("server name = %v, want llamacloud-postgres", srv.Name)
	}
	if len(tr.seen) != 1 {
		t.Fatalf("sent %d requests, want 1", len(tr.seen))
	}
	if path := tr.seen[0].URL.Path; !strings.Contains(path, "/resourceGroups/rg-1/") || !strings.Contains(path, "/flexibleServers/llamacloud-postgres") {
		t.Errorf("request path = %q, want it to address the named resource group and server", path)
	}
}

// TestAzurePGGetServerNotFound is the create-or-adopt hinge: a 404 must map to
// found=false with NO error, so Ensure creates rather than aborting.
func TestAzurePGGetServerNotFound(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusNotFound, body: `{"error":{"code":"ResourceNotFound","message":"not found"}}`}

	_, found, err := newPGAdapter(t, tr).GetServer(context.Background(), "rg-1", "llamacloud-postgres")
	if err != nil {
		t.Fatalf("a 404 must not be an error (it means 'create it'), got %v", err)
	}
	if found {
		t.Error("found = true, want false for a 404")
	}
}

// TestAzurePGGetServerOtherErrorPropagates proves a non-404 failure (a
// permission problem) is surfaced rather than silently read as "does not exist"
// — mistaking Forbidden for absent would make Ensure try to create a server that
// is already there.
func TestAzurePGGetServerOtherErrorPropagates(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusForbidden, body: `{"error":{"code":"AuthorizationFailed","message":"denied"}}`}

	_, found, err := newPGAdapter(t, tr).GetServer(context.Background(), "rg-1", "llamacloud-postgres")
	if err == nil {
		t.Fatal("a 403 must propagate as an error, not be read as 'not found'")
	}
	if found {
		t.Error("found = true alongside an error")
	}
	if !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("error = %q, want the ARM error code preserved", err)
	}
}

// TestAzurePGCreateServerSendsSpec proves the create adapter PUTs the server
// spec it was handed and collapses the long-running-operation poller to an
// error-only result.
func TestAzurePGCreateServerSendsSpec(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"name":"llamacloud-postgres","location":"eastus"}`}
	loc := "eastus"

	err := newPGAdapter(t, tr).CreateServer(context.Background(), "rg-1", "llamacloud-postgres", armpg.Server{Location: &loc})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if len(tr.seen) != 1 {
		t.Fatalf("sent %d requests, want 1", len(tr.seen))
	}
	if tr.seen[0].Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", tr.seen[0].Method)
	}
}

// TestAzurePGCreateServerErrorPropagates proves a rejected create is surfaced.
func TestAzurePGCreateServerErrorPropagates(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusBadRequest, body: `{"error":{"code":"InvalidParameter","message":"bad sku"}}`}

	err := newPGAdapter(t, tr).CreateServer(context.Background(), "rg-1", "pg", armpg.Server{})
	if err == nil {
		t.Fatal("a rejected create must return an error")
	}
	if !strings.Contains(err.Error(), "InvalidParameter") {
		t.Errorf("error = %q, want the ARM error code preserved", err)
	}
}

// TestAzurePGDeleteServer proves the delete adapter issues a DELETE and collapses
// its poller.
func TestAzurePGDeleteServer(t *testing.T) {
	// ARM answers a completed delete with 204 No Content.
	tr := &scriptedTransport{status: http.StatusNoContent}

	if err := newPGAdapter(t, tr).DeleteServer(context.Background(), "rg-1", "llamacloud-postgres"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if len(tr.seen) != 1 || tr.seen[0].Method != http.MethodDelete {
		t.Fatalf("want a single DELETE, got %d requests (%s)", len(tr.seen), methodsOf(tr))
	}
}

// TestAzurePGCreateDatabase proves the logical-database adapter addresses the
// named server and database.
func TestAzurePGCreateDatabase(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"name":"llamacloud"}`}

	if err := newPGAdapter(t, tr).CreateDatabase(context.Background(), "rg-1", "llamacloud-postgres", "llamacloud"); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if len(tr.seen) != 1 {
		t.Fatalf("sent %d requests, want 1", len(tr.seen))
	}
	if path := tr.seen[0].URL.Path; !strings.HasSuffix(path, "/databases/llamacloud") {
		t.Errorf("request path = %q, want it to address the llamacloud database", path)
	}
}

// TestAzureRedisGetClusterFound proves an existing AMR cluster decodes with
// found=true.
func TestAzureRedisGetClusterFound(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"name":"llamacloud-redis","location":"eastus"}`}

	cl, found, err := newRedisAdapter(t, tr).GetCluster(context.Background(), "rg-1", "llamacloud-redis")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if cl.Name == nil || *cl.Name != "llamacloud-redis" {
		t.Errorf("cluster name = %v, want llamacloud-redis", cl.Name)
	}
}

// TestAzureRedisGetClusterNotFound proves the 404-to-create hinge for Redis too.
func TestAzureRedisGetClusterNotFound(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusNotFound, body: `{"error":{"code":"ResourceNotFound"}}`}

	_, found, err := newRedisAdapter(t, tr).GetCluster(context.Background(), "rg-1", "llamacloud-redis")
	if err != nil {
		t.Fatalf("a 404 must not be an error, got %v", err)
	}
	if found {
		t.Error("found = true, want false")
	}
}

// TestAzureRedisGetClusterErrorPropagates proves non-404 failures surface.
func TestAzureRedisGetClusterErrorPropagates(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusForbidden, body: `{"error":{"code":"AuthorizationFailed"}}`}

	if _, _, err := newRedisAdapter(t, tr).GetCluster(context.Background(), "rg-1", "c"); err == nil {
		t.Fatal("a 403 must propagate")
	}
}

// TestAzureRedisCreateAndDeleteCluster proves the cluster lifecycle adapters
// issue PUT and DELETE and collapse their pollers.
func TestAzureRedisCreateAndDeleteCluster(t *testing.T) {
	create := &scriptedTransport{status: http.StatusOK, body: `{"name":"llamacloud-redis"}`}
	if err := newRedisAdapter(t, create).CreateCluster(context.Background(), "rg-1", "llamacloud-redis", armredis.Cluster{}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if len(create.seen) != 1 || create.seen[0].Method != http.MethodPut {
		t.Errorf("create: want a single PUT, got %s", methodsOf(create))
	}

	del := &scriptedTransport{status: http.StatusOK}
	if err := newRedisAdapter(t, del).DeleteCluster(context.Background(), "rg-1", "llamacloud-redis"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if len(del.seen) != 1 || del.seen[0].Method != http.MethodDelete {
		t.Errorf("delete: want a single DELETE, got %s", methodsOf(del))
	}
}

// TestAzureRedisCreateClusterErrorPropagates proves a rejected cluster create
// surfaces.
func TestAzureRedisCreateClusterErrorPropagates(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusBadRequest, body: `{"error":{"code":"InvalidSku"}}`}

	if err := newRedisAdapter(t, tr).CreateCluster(context.Background(), "rg-1", "c", armredis.Cluster{}); err == nil {
		t.Fatal("a rejected cluster create must return an error")
	}
}

// TestAzureRedisGetDatabase covers the database read in both shapes: present,
// and 404 mapped to found=false.
func TestAzureRedisGetDatabase(t *testing.T) {
	found := &scriptedTransport{status: http.StatusOK, body: `{"name":"default"}`}
	db, ok, err := newRedisAdapter(t, found).GetDatabase(context.Background(), "rg-1", "llamacloud-redis", "default")
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	if !ok {
		t.Fatal("found = false, want true")
	}
	if db.Name == nil || *db.Name != "default" {
		t.Errorf("database name = %v, want default", db.Name)
	}

	absent := &scriptedTransport{status: http.StatusNotFound, body: `{"error":{"code":"ResourceNotFound"}}`}
	if _, ok, err := newRedisAdapter(t, absent).GetDatabase(context.Background(), "rg-1", "c", "default"); err != nil || ok {
		t.Errorf("a 404 should be (found=false, err=nil), got (found=%v, err=%v)", ok, err)
	}

	denied := &scriptedTransport{status: http.StatusForbidden, body: `{"error":{"code":"AuthorizationFailed"}}`}
	if _, _, err := newRedisAdapter(t, denied).GetDatabase(context.Background(), "rg-1", "c", "default"); err == nil {
		t.Error("a 403 must propagate")
	}
}

// TestAzureRedisCreateDatabase proves the database create addresses the named
// database under the cluster.
func TestAzureRedisCreateDatabase(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"name":"default"}`}

	if err := newRedisAdapter(t, tr).CreateDatabase(context.Background(), "rg-1", "llamacloud-redis", "default", armredis.Database{}); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if path := tr.seen[0].URL.Path; !strings.HasSuffix(path, "/databases/default") {
		t.Errorf("request path = %q, want it to address the default database", path)
	}
}

// TestAzureRedisEnableAccessKeysSendsEnabled proves the update adapter sends the
// AccessKeysAuthentication=Enabled property — that flag is what makes the
// ListKeys credential flow work at all, so a silently-empty body would leave the
// chart with no way to authenticate.
func TestAzureRedisEnableAccessKeysSendsEnabled(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"name":"default"}`}

	if err := newRedisAdapter(t, tr).EnableAccessKeys(context.Background(), "rg-1", "llamacloud-redis", "default"); err != nil {
		t.Fatalf("EnableAccessKeys: %v", err)
	}
	if len(tr.seen) != 1 {
		t.Fatalf("sent %d requests, want 1", len(tr.seen))
	}
	body := readRequestBody(t, tr.seen[0])
	var payload struct {
		Properties struct {
			AccessKeysAuthentication string `json:"accessKeysAuthentication"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("request body did not parse: %v (%s)", err, body)
	}
	if payload.Properties.AccessKeysAuthentication != string(armredis.AccessKeysAuthenticationEnabled) {
		t.Errorf("accessKeysAuthentication = %q, want %q",
			payload.Properties.AccessKeysAuthentication, armredis.AccessKeysAuthenticationEnabled)
	}
}

// TestAzureRedisListKeysReturnsPrimary proves the primary access key is
// extracted from the response — that string becomes the chart's Redis password.
func TestAzureRedisListKeysReturnsPrimary(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"primaryKey":"primary-secret","secondaryKey":"secondary-secret"}`}

	key, err := newRedisAdapter(t, tr).ListKeys(context.Background(), "rg-1", "llamacloud-redis", "default")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if key != "primary-secret" {
		t.Errorf("key = %q, want the PRIMARY key", key)
	}
}

// TestAzureRedisListKeysMissingPrimary proves a response with no primary key
// yields an empty string rather than a nil dereference — the caller decides what
// an empty credential means.
func TestAzureRedisListKeysMissingPrimary(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusOK, body: `{"secondaryKey":"only-secondary"}`}

	key, err := newRedisAdapter(t, tr).ListKeys(context.Background(), "rg-1", "c", "default")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if key != "" {
		t.Errorf("key = %q, want empty when no primary key is returned", key)
	}
}

// TestAzureRedisListKeysErrorPropagates proves a failed key listing surfaces
// instead of returning an empty credential the chart would then use.
func TestAzureRedisListKeysErrorPropagates(t *testing.T) {
	tr := &scriptedTransport{status: http.StatusForbidden, body: `{"error":{"code":"AuthorizationFailed"}}`}

	key, err := newRedisAdapter(t, tr).ListKeys(context.Background(), "rg-1", "c", "default")
	if err == nil {
		t.Fatal("a failed ListKeys must return an error, not an empty credential")
	}
	if key != "" {
		t.Errorf("key = %q, want empty alongside the error", key)
	}
}

// methodsOf renders the HTTP methods a scripted transport saw, for assertions.
func methodsOf(tr *scriptedTransport) string {
	var parts []string
	for _, r := range tr.seen {
		parts = append(parts, r.Method)
	}
	return strings.Join(parts, ",")
}

// readRequestBody reads a recorded request's body.
func readRequestBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	if req.Body == nil {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return body
}
