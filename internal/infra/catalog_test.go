package infra

import (
	"strings"
	"testing"
)

// TestSteps_ManagedRequestsNoNetworkingPermissions PINS the zero-networking
// contract at the permission-catalog boundary: because managed provisioning
// creates no subnet group, no security group and no ingress rule, the catalog must
// request the RDS/ElastiCache create+describe actions and NOTHING networking-shaped
// — no provision-managed-network step, no ec2:* action anywhere, and no
// subnet-group action on either kind.
//
// This is the test that goes red if a future change quietly reintroduces
// CLI-side networking: the catalog is derived from the provisioners' AWS surfaces,
// so a new networking call has to be declared here to work at all.
func TestSteps_ManagedRequestsNoNetworkingPermissions(t *testing.T) {
	steps := Steps(Plan{Namespace: "acme", Region: "us-east-1", Managed: []string{"postgres", "redis"}})

	byID := map[string]Step{}
	for _, s := range steps {
		byID[s.ID] = s
	}

	if _, ok := byID["provision-managed-network"]; ok {
		t.Error("catalog still emits a provision-managed-network step; the CLI creates no networking")
	}

	for _, tc := range []struct {
		id   string
		want []string
	}{
		{"provision-managed-postgres", []string{"rds:DescribeDBInstances", "rds:CreateDBInstance"}},
		{"provision-managed-redis", []string{"elasticache:DescribeReplicationGroups", "elasticache:CreateReplicationGroup"}},
	} {
		step, ok := byID[tc.id]
		if !ok {
			t.Errorf("catalog missing the %s step", tc.id)
			continue
		}
		if len(step.Actions) != len(tc.want) {
			t.Errorf("%s actions = %v, want exactly %v", tc.id, step.Actions, tc.want)
			continue
		}
		for i, want := range tc.want {
			if step.Actions[i] != want {
				t.Errorf("%s actions = %v, want exactly %v", tc.id, step.Actions, tc.want)
				break
			}
		}
	}

	// No step anywhere may request an EC2 action or a subnet-group action. The
	// access-eks-cluster step's eks:DescribeCluster legitimately remains: it is for
	// generating a kubeconfig, not for discovering networking.
	for _, s := range steps {
		for _, a := range s.Actions {
			if strings.HasPrefix(a, "ec2:") {
				t.Errorf("step %s requests EC2 action %q; the CLI touches no networking", s.ID, a)
			}
			if strings.Contains(a, "SubnetGroup") {
				t.Errorf("step %s requests subnet-group action %q; the CLI creates no subnet group", s.ID, a)
			}
		}
	}
}
