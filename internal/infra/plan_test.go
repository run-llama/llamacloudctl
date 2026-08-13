package infra

import (
	"reflect"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/values"
)

func TestFromValues_BucketExtraction(t *testing.T) {
	v := values.FromMap(map[string]any{
		"config": map[string]any{
			"storageBuckets": map[string]any{
				"provider":        "aws", // must be excluded
				"parsedDocuments": "acme-docs",
				"parseFileOutput": "acme-output",
				"extractOutput":   "acme-docs", // duplicate -> deduped
				"s3proxy": map[string]any{ // nested map -> skipped
					"enabled": true,
				},
			},
		},
	})
	p := FromValues(v, "acme", "us-west-2")
	want := []string{"acme-docs", "acme-output"} // sorted + deduped, provider excluded
	if !reflect.DeepEqual(p.Buckets, want) {
		t.Fatalf("buckets = %v, want %v", p.Buckets, want)
	}
	if p.Namespace != "acme" {
		t.Errorf("namespace = %q, want acme", p.Namespace)
	}
	if p.Region != "us-west-2" {
		t.Errorf("region = %q, want us-west-2", p.Region)
	}
}

func TestFromValues_DefaultBucketWhenNoneConfigured(t *testing.T) {
	p := FromValues(values.FromMap(map[string]any{}), "myns", "")
	want := []string{"myns-storage"}
	if !reflect.DeepEqual(p.Buckets, want) {
		t.Fatalf("buckets = %v, want %v", p.Buckets, want)
	}
}

func TestFromValues_EmptyNamespaceDefaultsToLlamacloud(t *testing.T) {
	p := FromValues(values.FromMap(map[string]any{}), "", "")
	if p.Namespace != "llamacloud" {
		t.Errorf("namespace = %q, want llamacloud", p.Namespace)
	}
	if p.Buckets[0] != "llamacloud-storage" {
		t.Errorf("bucket = %q, want llamacloud-storage", p.Buckets[0])
	}
}

func TestResolveRegion_EnvFallback(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-central-1")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if got := resolveRegion(""); got != "eu-central-1" {
		t.Errorf("resolveRegion(env) = %q, want eu-central-1", got)
	}
	if got := resolveRegion("ap-south-1"); got != "ap-south-1" {
		t.Errorf("explicit region should win, got %q", got)
	}
}

func TestResolveRegion_DefaultWhenNothingSet(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if got := resolveRegion(""); got != defaultRegion {
		t.Errorf("resolveRegion() = %q, want %q", got, defaultRegion)
	}
}
