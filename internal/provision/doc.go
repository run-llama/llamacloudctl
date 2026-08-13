// Package provision is the home of the per-data-service provisioning seam
// (the managed-provisioner contract). The Provisioner interface (provisioner.go) and the concrete AWS
// provisioners — RDS Postgres (rds.go) and ElastiCache Redis (elasticache.go),
// with their mockable AWS surfaces (aws.go) and credential primitives (creds.go)
// — are published here.
//
// # The seam
//
// A Provisioner encapsulates one data service (for example PostgreSQL, object
// storage, a message queue): how to ensure it exists for an installation, and
// what Helm values fragment wires the rest of the chart to it. The install
// wizard obtains one Provisioner per data service the customer selects and,
// for each, calls the seam in two steps:
//
//	Ensure           — make the data service ready (create/verify the backing
//	                    resource, or validate a customer-provided external one).
//	ValuesFragment   — return the values fragment to merge into the release
//	                    values (via internal/values) so the chart points at it.
//
// This lets the wizard treat "bundled" and "external" data services uniformly:
// each is just a Provisioner that yields a values fragment.
//
// # Scope
//
// This package is a LEAF: it must NOT import internal/install (install consumes
// provision, never the reverse). It covers the AWS RDS Postgres and ElastiCache
// Redis managed data services; non-AWS clouds are follow-ups.
package provision
