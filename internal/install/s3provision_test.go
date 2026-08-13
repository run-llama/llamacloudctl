package install

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is a scripted s3CreateAPI that records the CreateBucket inputs it saw
// and returns a canned error, so the create-if-absent logic is exercised without
// a live AWS account.
type fakeS3 struct {
	err    error
	inputs []*s3.CreateBucketInput
}

func (f *fakeS3) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.inputs = append(f.inputs, in)
	return &s3.CreateBucketOutput{}, f.err
}

func TestCreateBucketIfAbsent_Success(t *testing.T) {
	f := &fakeS3{}
	if err := createBucketIfAbsent(context.Background(), f, "my-bucket", "us-west-2"); err != nil {
		t.Fatalf("createBucketIfAbsent: %v", err)
	}
	if len(f.inputs) != 1 || f.inputs[0].Bucket == nil || *f.inputs[0].Bucket != "my-bucket" {
		t.Fatalf("expected one CreateBucket for my-bucket, got %+v", f.inputs)
	}
}

func TestCreateBucketIfAbsent_AlreadyOwnedIsSuccess(t *testing.T) {
	// BucketAlreadyOwnedByYou is the idempotent re-run case: treated as success.
	f := &fakeS3{err: &s3types.BucketAlreadyOwnedByYou{}}
	if err := createBucketIfAbsent(context.Background(), f, "owned-bucket", "us-east-1"); err != nil {
		t.Errorf("already-owned bucket should be success, got: %v", err)
	}
}

func TestCreateBucketIfAbsent_AlreadyExistsIsError(t *testing.T) {
	// BucketAlreadyExists = owned by another account: a genuine, actionable error.
	f := &fakeS3{err: &s3types.BucketAlreadyExists{}}
	err := createBucketIfAbsent(context.Background(), f, "taken-bucket", "us-east-1")
	if err == nil {
		t.Fatal("expected an error for a name owned by another account, got nil")
	}
	var exists *s3types.BucketAlreadyExists
	if !errors.As(err, &exists) {
		t.Errorf("error should wrap BucketAlreadyExists, got: %v", err)
	}
}

func TestCreateBucketIfAbsent_OtherErrorPropagates(t *testing.T) {
	sentinel := errors.New("access denied")
	f := &fakeS3{err: sentinel}
	if err := createBucketIfAbsent(context.Background(), f, "b", "us-east-1"); !errors.Is(err, sentinel) {
		t.Errorf("expected the underlying error to propagate, got: %v", err)
	}
}

// TestCreateBucketIfAbsent_RegionConstraint pins the us-east-1 special case: no
// LocationConstraint there, but a constraint for every other region.
func TestCreateBucketIfAbsent_RegionConstraint(t *testing.T) {
	t.Run("us-east-1 omits LocationConstraint", func(t *testing.T) {
		f := &fakeS3{}
		if err := createBucketIfAbsent(context.Background(), f, "b", "us-east-1"); err != nil {
			t.Fatalf("createBucketIfAbsent: %v", err)
		}
		if f.inputs[0].CreateBucketConfiguration != nil {
			t.Errorf("us-east-1 must NOT set a LocationConstraint, got %+v", f.inputs[0].CreateBucketConfiguration)
		}
	})
	t.Run("non-default region sets LocationConstraint", func(t *testing.T) {
		f := &fakeS3{}
		if err := createBucketIfAbsent(context.Background(), f, "b", "eu-central-1"); err != nil {
			t.Fatalf("createBucketIfAbsent: %v", err)
		}
		cfg := f.inputs[0].CreateBucketConfiguration
		if cfg == nil {
			t.Fatal("non-default region must set a LocationConstraint")
		}
		if cfg.LocationConstraint != s3types.BucketLocationConstraint("eu-central-1") {
			t.Errorf("LocationConstraint = %q, want eu-central-1", cfg.LocationConstraint)
		}
	})
	t.Run("empty region omits LocationConstraint", func(t *testing.T) {
		f := &fakeS3{}
		if err := createBucketIfAbsent(context.Background(), f, "b", ""); err != nil {
			t.Fatalf("createBucketIfAbsent: %v", err)
		}
		if f.inputs[0].CreateBucketConfiguration != nil {
			t.Errorf("empty region must not set a LocationConstraint")
		}
	})
}
