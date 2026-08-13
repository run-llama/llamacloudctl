package infra

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// This file guards the managed-Terraform emitter's inputs: managed data services
// are a per-cloud CAPABILITY (RDS/ElastiCache on AWS, Cloud SQL/Memorystore on
// GCP — see cloud.SupportsManaged), and the namespace must be usable as-is inside
// the derived cloud identifiers the emitter names its resources with.

// ValidateManagedCloud rejects a managed-emission request against a cloud that has
// no managed provisioners. No managed kinds means nothing to guard; an empty cloud
// means the historical AWS default. The supported set is read from
// cloud.ManagedCapableNames so this message stays accurate as clouds land instead
// of hard-coding a list that rots.
func ValidateManagedCloud(c cloud.Cloud, kinds []provision.Kind) error {
	if len(kinds) == 0 {
		return nil
	}
	if c == "" || c.SupportsManaged() {
		return nil
	}
	supported := strings.Join(cloud.ManagedCapableNames(), ", ")
	return fmt.Errorf("managed data services are not supported on cloud %q: supported clouds are %s; re-run with one of those or drop --managed", c, supported)
}

// managedNamespacePattern accepts a lowercase-letter-first string of lowercase
// letters, digits, and single interior hyphens with no trailing hyphen. The syntax
// rules are IDENTICAL on every managed-capable cloud, so they stay one code path.
var managedNamespacePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// managedNamespaceRules is the per-cloud namespace bound and the service that
// BINDS it. The redis identifier is the tightest on every cloud, so redis names
// the bound everywhere — but the numeric cap differs, and the error text must
// name the RIGHT service or the operator is sent to the wrong provider's
// documentation.
//
// AWS and GCP coincide at 23: their redis cap is 40 characters and
// provision.ManagedIdentifier appends the same 17-character "-llamacloud-redis"
// suffix (40 - 17 = 23). Azure is LOOSER at 43, because an Azure Managed Redis
// cluster name may run to 60 characters (60 - 17 = 43); no namespace that passes
// the AWS/GCP bound can fail the Azure one.
//
// (The postgres side never binds. On AWS an RDS instance identifier allows 63; on
// GCP the Cloud SQL instance-id cap is 98; on Azure a PostgreSQL Flexible Server
// name allows 63, and the worst case an Azure namespace can produce is
// 43 + len("-llamacloud-pg") = 57.)
type managedNamespaceRule struct {
	maxNamespace int
	bindingSvc   string
	bindingLimit int
}

var managedNamespaceRules = map[cloud.Cloud]managedNamespaceRule{
	cloud.AWS:   {maxNamespace: 23, bindingSvc: "ElastiCache replication group", bindingLimit: 40},
	cloud.Azure: {maxNamespace: 43, bindingSvc: "Azure Managed Redis cluster", bindingLimit: 60},
	cloud.GCP:   {maxNamespace: 23, bindingSvc: "Memorystore instance", bindingLimit: 40},
}

// managedNamespaceMaxLen is the bound applied when the cloud is unresolved; it
// equals the AWS rule, matching the empty-cloud-means-AWS convention used
// throughout.
const managedNamespaceMaxLen = 23

// ruleFor resolves the namespace rule for a cloud, defaulting to the AWS rule for
// an empty or unknown cloud (the historical behaviour everywhere else).
func ruleFor(c cloud.Cloud) managedNamespaceRule {
	if r, ok := managedNamespaceRules[c]; ok {
		return r
	}
	return managedNamespaceRules[cloud.AWS]
}

// ValidateManagedNamespace rejects a namespace that cannot be embedded in the
// managed resource identifiers the emitter derives from it (see
// managedNamespaceRules for the per-cloud bound): it must start with a lowercase
// letter, contain only [a-z0-9-], contain no "--", not end with a hyphen, and fit
// the cloud's length bound. Empty is invalid.
//
// It REJECTS rather than truncates, deliberately diverging from sanitizeAccountID
// (terraform_gcp.go), which truncates a too-long namespace into a GSA account_id.
// The asymmetry is correct: a GSA account_id is cosmetic, whereas the managed
// identifier must be byte-identical between provision.ManagedIdentifier (the SDK's
// create-or-adopt key) and the emitted HCL — silently truncating on one path would
// break create-or-adopt against a Terraform-created instance. sanitizeAccountID is
// the cited idiom for the SHAPE of the character normalization, not its truncation.
func ValidateManagedNamespace(c cloud.Cloud, ns string) error {
	rule := ruleFor(c)
	if ns == "" {
		return fmt.Errorf("managed data services need a namespace: the %s identifiers are derived from it", rule.bindingSvc)
	}
	if len(ns) > rule.maxNamespace {
		return fmt.Errorf("namespace %q is too long for managed data services: max %d characters, so the derived identifier %q stays within the %d-character %s limit", ns, rule.maxNamespace, provision.ManagedIdentifier(ns, provision.KindRedis), rule.bindingLimit, rule.bindingSvc)
	}
	if !managedNamespacePattern.MatchString(ns) {
		return fmt.Errorf("namespace %q is not usable in managed cloud identifiers: it must start with a lowercase letter and contain only lowercase letters, digits, and hyphens", ns)
	}
	if strings.Contains(ns, "--") {
		return fmt.Errorf("namespace %q is not usable in managed cloud identifiers: consecutive hyphens (\"--\") are not allowed", ns)
	}
	if ns[len(ns)-1] == '-' {
		return fmt.Errorf("namespace %q is not usable in managed cloud identifiers: it must not end with a hyphen", ns)
	}
	return nil
}
