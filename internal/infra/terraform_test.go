package infra

import (
	"strings"
	"testing"
)

func TestTerraform_ContainsBucketsAndIRSA(t *testing.T) {
	p := Plan{Namespace: "acme", Region: "us-west-2", Buckets: []string{"acme-docs", "acme-output"}}
	tf := Terraform(p)

	for _, want := range []string{
		`provider "aws"`,
		`region = "us-west-2"`,
		`bucket = "acme-docs"`,
		`bucket = "acme-output"`,
		`resource "aws_iam_policy" "llama_s3"`,
		`resource "aws_iam_role" "llama_irsa"`,
		`resource "aws_iam_role_policy_attachment" "llama_s3"`,
		`sts:AssumeRoleWithWebIdentity`,
		`system:serviceaccount:${var.k8s_namespace}:*`,
		`output "irsa_role_arn"`,
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("terraform output missing %q", want)
		}
	}

	// Runtime actions are granted, not the install-time CreateBucket.
	for _, a := range RuntimeS3Actions {
		if !strings.Contains(tf, a) {
			t.Errorf("terraform IRSA policy missing runtime action %q", a)
		}
	}
	if strings.Contains(tf, "s3:CreateBucket") {
		t.Errorf("terraform must not grant install-time s3:CreateBucket to the pods")
	}
}

func TestTerraform_Deterministic(t *testing.T) {
	p := Plan{Namespace: "acme", Region: "us-east-1", Buckets: []string{"b1", "b2"}}
	first := Terraform(p)
	second := Terraform(p)
	if first != second {
		t.Fatal("Terraform output is not deterministic across identical calls")
	}
}

func TestTerraform_CollidingLocalNamesAreUnique(t *testing.T) {
	// Two names that sanitize to the same identifier must get distinct locals.
	p := Plan{Namespace: "ns", Region: "us-east-1", Buckets: []string{"a.b", "a-b"}}
	tf := Terraform(p)
	if strings.Count(tf, `resource "aws_s3_bucket" "bucket_a_b"`) != 1 {
		t.Errorf("expected exactly one bucket_a_b local, got:\n%s", tf)
	}
	if !strings.Contains(tf, `bucket_a_b_2`) {
		t.Errorf("expected a de-collided bucket_a_b_2 local, got:\n%s", tf)
	}
}

func TestSanitizeLocal(t *testing.T) {
	cases := map[string]string{
		"Acme-Docs.Prod": "acme_docs_prod",
		"simple":         "simple",
		"a.b/c":          "a_b_c",
	}
	for in, want := range cases {
		if got := sanitizeLocal(in); got != want {
			t.Errorf("sanitizeLocal(%q) = %q, want %q", in, got, want)
		}
	}
}
