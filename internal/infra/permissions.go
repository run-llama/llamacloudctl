package infra

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// simulatorAPI is the minimal IAM surface the check needs: can this principal
// perform these actions on these resources?
type simulatorAPI interface {
	SimulatePrincipalPolicy(ctx context.Context, in *iam.SimulatePrincipalPolicyInput, optFns ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error)
}

// CheckResult is the outcome of a permission check: the resolved caller identity,
// a per-step report, and whether the actions were actually probed live (false =
// we fell back to the static required-actions catalog because AWS was unreachable
// or the simulation itself was denied).
type CheckResult struct {
	CallerARN string
	Live      bool
	Report    *report.Report
	Missing   []string // union of actions that were denied or could not be verified
}

// CheckLive resolves the caller's AWS identity and probes each install step with
// IAM SimulatePrincipalPolicy, returning a per-step report. If AWS config or the
// caller identity cannot be resolved it returns a static CheckResult (Live=false)
// that still tells the operator exactly what each step requires. It never returns
// an error — an unreachable AWS is itself a reportable outcome, not a crash.
func CheckLive(ctx context.Context, p Plan) *CheckResult {
	// Live per-action simulation exists only for AWS (IAM SimulatePrincipalPolicy).
	// Azure and GCP have no equivalent wired here, so their permission surface is
	// reported via the static required-roles catalog — the operator still sees
	// exactly what each step needs and can hand off the emitted Terraform.
	if p.EffectiveCloud() != cloud.AWS {
		return staticResult(p, fmt.Sprintf("live permission simulation is implemented for AWS only; listing the %s roles/permissions each step requires (hand `llamacloudctl terraform --cloud %s` to your infra team to provision them)", p.EffectiveCloud(), p.EffectiveCloud()))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(p.Region))
	if err != nil {
		return staticResult(p, fmt.Sprintf("could not load AWS config (%v)", err))
	}
	ident := sts.NewFromConfig(cfg)
	out, err := ident.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return staticResult(p, fmt.Sprintf("could not determine your AWS identity (%v); is AWS configured?", err))
	}
	arn := aws.ToString(out.Arn)
	return Check(ctx, p, arn, iam.NewFromConfig(cfg))
}

// Check is the injectable core: given the caller ARN and an IAM simulator, it
// evaluates every step and builds the report. Exposed (with interface params) so
// tests drive it with fakes and no live AWS.
func Check(ctx context.Context, p Plan, callerARN string, sim simulatorAPI) *CheckResult {
	res := &CheckResult{CallerARN: callerARN, Live: true, Report: &report.Report{}}
	source := principalARN(callerARN)
	missing := map[string]bool{}

	for _, step := range Steps(p) {
		decisions, simErr := simulateStep(ctx, sim, source, step)
		if simErr != nil {
			// The simulation itself failed (commonly iam:SimulatePrincipalPolicy is
			// denied). Degrade to the static catalog for this step rather than
			// claiming a false DENY.
			res.Live = false
			res.Report.Add(report.Result{
				Name:        step.Name,
				Severity:    report.WARNING,
				Message:     step.Detail + " — could not verify live.",
				Detail:      staticStepDetail(step) + fmt.Sprintf("\n        (simulation failed: %v)", simErr),
				Remediation: staticStepRemediation(step),
			})
			// The WARNING row above still renders; only the aggregate Missing /
			// request-policy view skips a SkipRequestPolicy step (an inferred
			// teardown kind must not inflate the policy the operator requests).
			if !step.SkipRequestPolicy {
				for _, a := range step.Actions {
					missing[a] = true
				}
			}
			continue
		}
		res.Report.Add(stepResult(step, decisions, missing))
	}

	res.Missing = sortedKeys(missing)
	return res
}

// simulateStep runs SimulatePrincipalPolicy for one step and returns, per action,
// whether it was allowed on ALL of the step's resources (an action is only
// "allowed" if no resource denies it).
func simulateStep(ctx context.Context, sim simulatorAPI, sourceARN string, step Step) (map[string]bool, error) {
	in := &iam.SimulatePrincipalPolicyInput{
		PolicySourceArn: aws.String(sourceARN),
		ActionNames:     step.Actions,
		ResourceArns:    step.Resources,
	}
	out, err := sim.SimulatePrincipalPolicy(ctx, in, func(o *iam.Options) {})
	if err != nil {
		return nil, err
	}
	// Seed every action as allowed, then flip to denied if any evaluation for it
	// is not "allowed". This aggregates the per-resource results the API returns.
	allowed := make(map[string]bool, len(step.Actions))
	for _, a := range step.Actions {
		allowed[a] = true
	}
	for _, r := range out.EvaluationResults {
		name := aws.ToString(r.EvalActionName)
		if r.EvalDecision != iamtypes.PolicyEvaluationDecisionTypeAllowed {
			allowed[name] = false
		}
	}
	return allowed, nil
}

// stepResult turns per-action allow/deny into a report Result, recording any
// denied actions into missing — except for a SkipRequestPolicy step, whose
// DENIED rows still render but stay out of the aggregate Missing / request-policy
// view. A non-optional step with a denied action FAILs; an optional step
// (prerequisite / mode-specific / inferred teardown) only WARNs.
func stepResult(step Step, allowed map[string]bool, missing map[string]bool) report.Result {
	var lines []string
	denied := 0
	for _, a := range step.Actions {
		if allowed[a] {
			lines = append(lines, fmt.Sprintf("  %s %s", pad(a), "ALLOWED"))
		} else {
			lines = append(lines, fmt.Sprintf("  %s %s", pad(a), "DENIED"))
			denied++
			if !step.SkipRequestPolicy {
				missing[a] = true
			}
		}
	}
	detail := strings.Join(lines, "\n        ")
	if step.Note != "" {
		detail += "\n        note: " + step.Note
	}

	switch {
	case denied == 0:
		return report.Result{
			Name:     step.Name,
			Severity: report.PASS,
			Message:  step.Detail,
			Detail:   detail,
		}
	case step.Optional:
		return report.Result{
			Name:        step.Name,
			Severity:    report.WARNING,
			Message:     fmt.Sprintf("%s — missing %d permission(s) for this optional step.", step.Detail, denied),
			Detail:      detail,
			Remediation: staticStepRemediation(step),
		}
	default:
		return report.Result{
			Name:        step.Name,
			Severity:    report.FAIL,
			Message:     fmt.Sprintf("%s — missing %d required permission(s).", step.Detail, denied),
			Detail:      detail,
			Remediation: staticStepRemediation(step),
		}
	}
}

// staticResult builds a Live=false CheckResult that lists what every step
// requires, used when the caller identity cannot be resolved at all. reason
// explains why no live probe ran.
func staticResult(p Plan, reason string) *CheckResult {
	res := &CheckResult{Live: false, Report: &report.Report{}}
	// AWS gets a live probe when creds resolve, so its skip-detail nudges the user
	// to configure AWS and re-run; Azure/GCP have no live probe at all, so the
	// static listing IS the answer, not a fallback to fix.
	detail := "Listing the permissions each step REQUIRES instead. Configure AWS credentials and re-run to verify them against your identity."
	if p.EffectiveCloud() != cloud.AWS {
		detail = "Listing the roles/permissions each step REQUIRES. Grant these (or apply the emitted Terraform) before installing."
	}
	res.Report.Add(report.Result{
		Name:     fmt.Sprintf("%s identity", strings.ToUpper(p.EffectiveCloud().String())),
		Severity: report.WARNING,
		Message:  "Skipped the live permission probe: " + reason,
		Detail:   detail,
	})
	missing := map[string]bool{}
	for _, step := range Steps(p) {
		sev := report.WARNING
		res.Report.Add(report.Result{
			Name:        step.Name,
			Severity:    sev,
			Message:     step.Detail,
			Detail:      staticStepDetail(step),
			Remediation: staticStepRemediation(step),
		})
		// Row rendered above regardless; a SkipRequestPolicy step (inferred
		// teardown kind) is still LISTED but never aggregated into Missing, so the
		// creds-less path cannot leak it into the request policy either.
		if !step.SkipRequestPolicy {
			for _, a := range step.Actions {
				missing[a] = true
			}
		}
	}
	res.Missing = sortedKeys(missing)
	return res
}

// staticStepDetail lists the required actions and the resources they apply to,
// used on both the no-identity and simulation-failed fallback paths.
func staticStepDetail(step Step) string {
	detail := "Requires: " + strings.Join(step.Actions, ", ")
	if len(step.Resources) > 0 {
		detail += "\n        on: " + strings.Join(step.Resources, ", ")
	}
	if step.Note != "" {
		detail += "\n        note: " + step.Note
	}
	return detail
}

// staticStepRemediation is the actionable ask for a step whose permissions are
// missing or unverified: request a role/policy granting its actions, or hand off
// the Terraform.
func staticStepRemediation(step Step) string {
	if step.ID == "provision-irsa" {
		return "Ask your infra team to apply `llamacloudctl terraform` output instead of creating the role yourself, or request an IAM role granting: " + strings.Join(step.Actions, ", ")
	}
	return "Ask an administrator for a role granting these actions, or have someone with it run this step. See the aggregate policy printed below."
}

// RequestPolicy renders a single IAM policy document that grants every action
// across all steps on the plan's resources — the JSON an operator hands to an
// administrator to request the access. Actions are grouped by their resource set
// so the document stays least-privilege rather than collapsing to Resource "*".
// SkipRequestPolicy steps (inferred teardown kinds) are excluded: the check
// reports on them, but the operator should not have to request permissions for
// resources the CLI merely GUESSED they have.
func RequestPolicy(p Plan) string {
	// Group actions by the joined resource list so identical-scope actions share a
	// statement. Deterministic: resource groups and actions are sorted.
	byResources := map[string][]string{}
	resourceOf := map[string][]string{}
	for _, step := range Steps(p) {
		if step.SkipRequestPolicy {
			continue
		}
		key := strings.Join(step.Resources, "\x00")
		resourceOf[key] = step.Resources
		byResources[key] = append(byResources[key], step.Actions...)
	}
	keys := sortedKeys(toSet(byResources))
	var statements []any
	for i, key := range keys {
		actions := dedupeSorted(byResources[key])
		statements = append(statements, map[string]any{
			"Sid":      fmt.Sprintf("LlamaCloudInstall%d", i),
			"Effect":   "Allow",
			"Action":   toAny(actions),
			"Resource": toAny(resourceOf[key]),
		})
	}
	doc := map[string]any{"Version": "2012-10-17", "Statement": statements}
	return marshalDoc(doc)
}

// RequestPermissions renders the SAME data RequestPolicy does — every action
// across all steps, grouped by resource set, with the same SkipRequestPolicy
// filter and the same deterministic sorting — as a PLAIN GROUPED LIST rather than
// an AWS IAM policy document.
//
// It exists because RequestPolicy emits a literal IAM policy document
// ({"Version":"2012-10-17","Statement":[...]}), which is meaningless off AWS.
// Printing that JSON under a cloud-neutral heading would put the words "not an
// IAM policy document" directly above one. The renderer differs; the permission
// model does not — this is a formatting sibling, not a second source of truth.
func RequestPermissions(p Plan) string {
	byResources := map[string][]string{}
	resourceOf := map[string][]string{}
	for _, step := range Steps(p) {
		if step.SkipRequestPolicy {
			continue
		}
		key := strings.Join(step.Resources, "\x00")
		resourceOf[key] = step.Resources
		byResources[key] = append(byResources[key], step.Actions...)
	}
	keys := sortedKeys(toSet(byResources))

	var b strings.Builder
	for _, key := range keys {
		resources := resourceOf[key]
		if len(resources) == 0 {
			resources = []string{"(project scope)"}
		}
		fmt.Fprintf(&b, "  on %s:\n", strings.Join(resources, ", "))
		for _, action := range dedupeSorted(byResources[key]) {
			fmt.Fprintf(&b, "    - %s\n", action)
		}
	}
	return b.String()
}

// principalARN normalizes a caller ARN into one SimulatePrincipalPolicy accepts.
// An STS assumed-role ARN (arn:aws:sts::acct:assumed-role/Role/session) is not a
// valid PolicySourceArn — it must be the underlying IAM role ARN
// (arn:aws:iam::acct:role/Role). User/role IAM ARNs pass through unchanged.
func principalARN(arn string) string {
	m := assumedRoleARN.FindStringSubmatch(arn)
	if m == nil {
		return arn
	}
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", m[1], m[2])
}

// assumedRoleARN captures the account id and role name from an STS assumed-role
// ARN. The session-name segment is discarded.
var assumedRoleARN = regexp.MustCompile(`^arn:aws:sts::(\d+):assumed-role/([^/]+)/`)

// pad right-justifies an action name to a fixed width so the ALLOWED/DENIED
// column lines up in the rendered report.
func pad(s string) string {
	const w = 26
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(".", w-len(s))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toSet(m map[string][]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func dedupeSorted(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
