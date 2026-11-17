package upgrade

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/getter"
)

// RepoFetcher's whole path — index download, version lookup, URL resolution,
// tarball download, archive load — runs against a LOCAL chart repository served
// from t.TempDir by httptest. No published repository is contacted.

// serveChartRepo packages a chart at the given version, writes an index.yaml
// referencing it by the supplied URL form, and serves both from a local server.
// Returns the repository base URL.
func serveChartRepo(t *testing.T, version, urlInIndex string) string {
	t.Helper()

	tgz := writeChartTgz(t, version)
	blob, err := os.ReadFile(tgz)
	if err != nil {
		t.Fatalf("read packaged chart: %v", err)
	}
	tgzName := filepath.Base(tgz)

	dir := t.TempDir()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ref := urlInIndex
	if ref == "" {
		ref = srv.URL + "/" + tgzName
	}
	index := "apiVersion: v1\nentries:\n  llamacloud:\n  - apiVersion: v2\n    name: llamacloud\n    version: " +
		version + "\n    urls:\n    - " + ref + "\ngenerated: \"2026-01-01T00:00:00Z\"\n"
	if err := os.WriteFile(filepath.Join(dir, "index.yaml"), []byte(index), 0o600); err != nil {
		t.Fatalf("write index.yaml: %v", err)
	}

	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(dir, "index.yaml"))
	})
	mux.HandleFunc("/"+tgzName, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(blob)
	})
	return srv.URL
}

// TestRepoFetcherFetchesFromRepository proves the full repository path returns
// the chart at the requested version with its schema and changelog attached —
// the three artifacts the upgrade's value migration and advisory both read.
func TestRepoFetcherFetchesFromRepository(t *testing.T) {
	repoURL := serveChartRepo(t, "0.9.0", "")
	rf := &RepoFetcher{RepoURL: repoURL}

	got, err := rf.Fetch("0.9.0")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Version != "0.9.0" {
		t.Errorf("version = %q, want 0.9.0", got.Version)
	}
	if got.Chart == nil || got.Chart.Name() != "llamacloud" {
		t.Errorf("chart = %+v, want the llamacloud chart", got.Chart)
	}
	if len(got.Schema) == 0 {
		t.Error("schema is empty; the values migration reads it")
	}
	if !strings.Contains(string(got.Changelog), "0.9.0") {
		t.Errorf("changelog = %q, want the packaged CHANGELOG.md", got.Changelog)
	}
}

// TestRepoFetcherResolvesRelativeChartURL proves a bare filename in the index
// (how several repositories publish) resolves against the repository base URL
// rather than failing to download.
func TestRepoFetcherResolvesRelativeChartURL(t *testing.T) {
	repoURL := serveChartRepo(t, "0.9.0", "llamacloud-0.9.0.tgz")
	rf := &RepoFetcher{RepoURL: repoURL}

	got, err := rf.Fetch("0.9.0")
	if err != nil {
		t.Fatalf("Fetch with a bare filename in the index: %v", err)
	}
	if got.Chart == nil {
		t.Error("chart was not loaded")
	}
}

// TestRepoFetcherUnknownVersionNamesIt proves a version absent from the index
// fails with the chart, version and repository in the message — that string is
// what an operator sees after a typo'd --chart-version.
func TestRepoFetcherUnknownVersionNamesIt(t *testing.T) {
	repoURL := serveChartRepo(t, "0.9.0", "")
	rf := &RepoFetcher{RepoURL: repoURL}

	_, err := rf.Fetch("9.9.9")
	if err == nil {
		t.Fatal("fetching an unpublished version should fail")
	}
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("error = %q, want it to name the requested version", err)
	}
	if !strings.Contains(err.Error(), repoURL) {
		t.Errorf("error = %q, want it to name the repository searched", err)
	}
}

// TestRepoFetcherUnreachableRepositoryNamesTheStep proves an index that cannot
// be downloaded fails naming the download step and the URL, rather than an
// opaque transport error.
func TestRepoFetcherUnreachableRepositoryNamesTheStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	rf := &RepoFetcher{RepoURL: srv.URL}

	_, err := rf.Fetch("0.9.0")
	if err == nil {
		t.Fatal("an unreachable repository index should fail")
	}
	if !strings.Contains(err.Error(), "download chart repo index") {
		t.Errorf("error = %q, want it to name the index download", err)
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error = %q, want it to name the repository URL", err)
	}
}

// TestRepoFetcherMissingTarballNamesTheChart proves an index entry whose
// tarball is not actually served fails naming the chart and version, not just
// the URL.
func TestRepoFetcherMissingTarballNamesTheChart(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	index := "apiVersion: v1\nentries:\n  llamacloud:\n  - apiVersion: v2\n    name: llamacloud\n    version: 0.9.0\n    urls:\n    - " +
		srv.URL + "/llamacloud-0.9.0.tgz\ngenerated: \"2026-01-01T00:00:00Z\"\n"
	if err := os.WriteFile(filepath.Join(dir, "index.yaml"), []byte(index), 0o600); err != nil {
		t.Fatalf("write index.yaml: %v", err)
	}
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(dir, "index.yaml"))
	})

	rf := &RepoFetcher{RepoURL: srv.URL}
	_, err := rf.Fetch("0.9.0")
	if err == nil {
		t.Fatal("a missing chart tarball should fail")
	}
	if !strings.Contains(err.Error(), "download chart llamacloud 0.9.0") {
		t.Errorf("error = %q, want it to name the chart and version", err)
	}
}

// TestRepoFetcherEntryWithoutURLs proves an index entry carrying no download URL
// is rejected with an explanation rather than panicking on an empty slice.
func TestRepoFetcherEntryWithoutURLs(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	index := "apiVersion: v1\nentries:\n  llamacloud:\n  - apiVersion: v2\n    name: llamacloud\n    version: 0.9.0\n    urls: []\ngenerated: \"2026-01-01T00:00:00Z\"\n"
	if err := os.WriteFile(filepath.Join(dir, "index.yaml"), []byte(index), 0o600); err != nil {
		t.Fatalf("write index.yaml: %v", err)
	}
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(dir, "index.yaml"))
	})

	rf := &RepoFetcher{RepoURL: srv.URL}
	_, err := rf.Fetch("0.9.0")
	if err == nil {
		t.Fatal("an index entry with no URLs should fail")
	}
	if !strings.Contains(err.Error(), "no download URL") {
		t.Errorf("error = %q, want it to explain the index entry is unusable", err)
	}
}

// TestResolveReference pins the chart-URL resolution rules Helm itself applies:
// an absolute URL is used as-is, a bare filename resolves inside the repository
// directory, and a relative path resolves against the repository base.
func TestResolveReference(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		ref     string
		want    string
		wantErr bool
	}{
		{
			name: "absolute URL passes through",
			base: "https://charts.example.com/repo",
			ref:  "https://cdn.example.com/llamacloud-0.9.0.tgz",
			want: "https://cdn.example.com/llamacloud-0.9.0.tgz",
		},
		{
			name: "bare filename resolves inside the repo directory",
			base: "https://charts.example.com/repo",
			ref:  "llamacloud-0.9.0.tgz",
			want: "https://charts.example.com/repo/llamacloud-0.9.0.tgz",
		},
		{
			name: "relative path resolves against the repo base",
			base: "https://charts.example.com/repo/index.yaml",
			ref:  "charts/llamacloud-0.9.0.tgz",
			want: "https://charts.example.com/repo/charts/llamacloud-0.9.0.tgz",
		},
		{
			name:    "unparseable repo URL errors",
			base:    "://broken",
			ref:     "llamacloud-0.9.0.tgz",
			wantErr: true,
		},
		{
			name:    "unparseable chart URL errors",
			base:    "https://charts.example.com/repo",
			ref:     "://also-broken",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveReference(tc.base, tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveReference(%q, %q) = %q, want an error", tc.base, tc.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveReference: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveReference(%q, %q) = %q, want %q", tc.base, tc.ref, got, tc.want)
			}
		})
	}
}

// TestDownloadUnsupportedScheme proves a chart URL whose scheme no getter
// handles fails naming the scheme, rather than silently returning empty bytes
// that would then fail as a corrupt archive.
func TestDownloadUnsupportedScheme(t *testing.T) {
	providers := getter.All(cli.New())

	_, err := download(providers, "ftp://charts.example.com/llamacloud-0.9.0.tgz")
	if err == nil {
		t.Fatal("an unsupported scheme should fail")
	}
	if !strings.Contains(err.Error(), `no getter for scheme "ftp"`) {
		t.Errorf("error = %q, want it to name the unsupported scheme", err)
	}
}

// TestDownloadUnparseableURL proves a malformed URL fails at parse with the URL
// quoted.
func TestDownloadUnparseableURL(t *testing.T) {
	providers := getter.All(cli.New())

	_, err := download(providers, "://not-a-url")
	if err == nil {
		t.Fatal("a malformed URL should fail")
	}
	if !strings.Contains(err.Error(), "parse download URL") {
		t.Errorf("error = %q, want it to name the parse step", err)
	}
}

// TestDownloadFetchesBytes proves the happy path returns the served bytes
// verbatim from a local server.
func TestDownloadFetchesBytes(t *testing.T) {
	const payload = "chart-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	got, err := download(getter.All(cli.New()), srv.URL+"/llamacloud-0.9.0.tgz")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(got) != payload {
		t.Errorf("download = %q, want %q", got, payload)
	}
}
