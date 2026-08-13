package install

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// The managed create is the longest wait in the whole flow: internal/provision's
// shared waiter (poll.go pollUntilReady) polls for up to 20 minutes and prints
// NOTHING, so a healthy RDS instance coming up was indistinguishable from a hung
// CLI. streamManagedProgress is the heartbeat that closes it.

// fastManagedProgress shortens the heartbeat interval for one test.
func fastManagedProgress(t *testing.T) {
	t.Helper()
	saved := managedProgressInterval
	managedProgressInterval = 5 * time.Millisecond
	t.Cleanup(func() { managedProgressInterval = saved })
}

// progressProvisioner is a minimal Provisioner whose only job is to carry the
// identity the heartbeat line renders.
type progressProvisioner struct {
	kind provision.Kind
	id   string
}

func (p progressProvisioner) Ensure(context.Context) (provision.Result, error) {
	return provision.Result{}, nil
}
func (p progressProvisioner) Kind() provision.Kind           { return p.kind }
func (p progressProvisioner) Identifier() string             { return p.id }
func (p progressProvisioner) Region() string                 { return "us-east-1" }
func (p progressProvisioner) Scope() string                  { return "" }
func (p progressProvisioner) ValuesFragment() map[string]any { return nil }

var _ provision.Provisioner = progressProvisioner{}

// TestStreamManagedProgress_NamesEachResourceAndElapsedTime proves the heartbeat
// identifies WHAT is being waited on — the concrete cloud service, the identifier
// and the region — rather than emitting an anonymous "still working" dot.
func TestStreamManagedProgress_NamesEachResourceAndElapsedTime(t *testing.T) {
	fastManagedProgress(t)
	managed := []provision.Provisioner{
		progressProvisioner{kind: provision.KindPostgres, id: "acme-llamacloud-pg"},
		progressProvisioner{kind: provision.KindRedis, id: "acme-llamacloud-redis"},
	}

	buf := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		streamManagedProgress(ctx, cloud.AWS, managed, buf)
		close(done)
	}()

	deadline := time.After(10 * time.Second)
	for !strings.Contains(buf.String(), "acme-llamacloud-redis") {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("no heartbeat naming both resources in 10s; wrote %q", buf.String())
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-done

	got := buf.String()
	for _, want := range []string{
		"still creating RDS PostgreSQL \"acme-llamacloud-pg\" in us-east-1",
		"still creating ElastiCache Redis \"acme-llamacloud-redis\" in us-east-1",
		"elapsed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("heartbeat missing %q; got:\n%s", want, got)
		}
	}
}

// TestStreamManagedProgress_UsesTheTargetCloudsServiceNames proves the line is
// per-cloud: a GCP install must not be told an RDS instance is being created.
func TestStreamManagedProgress_UsesTheTargetCloudsServiceNames(t *testing.T) {
	fastManagedProgress(t)
	managed := []provision.Provisioner{progressProvisioner{kind: provision.KindPostgres, id: "acme-llamacloud-pg"}}

	buf := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		streamManagedProgress(ctx, cloud.GCP, managed, buf)
		close(done)
	}()

	deadline := time.After(10 * time.Second)
	for !strings.Contains(buf.String(), "still creating") {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("no heartbeat in 10s")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if got := buf.String(); !strings.Contains(got, "Cloud SQL (PostgreSQL)") || strings.Contains(got, "RDS") {
		t.Errorf("the GCP heartbeat must name Cloud SQL, not RDS; got:\n%s", got)
	}
}

// TestStreamManagedProgress_NoProvisionersIsSilent guards against a stray
// heartbeat on a run with nothing managed.
func TestStreamManagedProgress_NoProvisionersIsSilent(t *testing.T) {
	fastManagedProgress(t)
	buf := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	streamManagedProgress(ctx, cloud.AWS, nil, buf)
	if buf.String() != "" {
		t.Errorf("an empty provisioner list must print nothing, got %q", buf.String())
	}
}
