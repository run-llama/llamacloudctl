package cli

import (
	"context"
	"fmt"

	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/install"
	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/preflight"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/values"
	"github.com/spf13/cobra"
)

// newPreflightCmd builds the `llamacloudctl preflight` command: every check the
// install flow can run, run on demand and read-only, so an operator can find out
// whether an install would work WITHOUT starting one.
//
// It is deliberately best-effort about its inputs. A missing kubeconfig, an
// unreachable cluster, or a chart that will not download degrades the checks that
// need them to WARNINGs rather than aborting the command — the whole point is to
// report as much as can be learned from wherever it is run.
func newPreflightCmd(opts *GlobalOptions) *cobra.Command {
	var (
		chartVersion string
		chartRef     string
		repoURL      string
		checkAWS     bool
		clusterName  string
		region       string
	)

	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Check whether this cluster and these values are ready for an install.",
		Long: "preflight runs every readiness check read-only: the chart values, the license\n" +
			"token, a full in-process render of the chart, the cluster's version and\n" +
			"capacity, reachability of the external databases, OIDC issuer discovery, and\n" +
			"whether the object-storage buckets the values name actually exist.\n" +
			"Nothing is installed and nothing is modified.\n\n" +
			"It exits non-zero when any check FAILs, so it is safe to gate a pipeline on.\n" +
			"Warnings never fail the run.\n\n" +
			"`llamacloudctl install` runs a trimmed version of these checks automatically;\n" +
			"this command is the full set, plus the AWS security-group cross-check under\n" +
			"--aws.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := values.Load(opts.ValueFiles...)
			if err != nil {
				return err
			}

			in := preflight.Input{
				Namespace:    opts.Namespace,
				ChartVersion: chartVersion,
				ValuesFiles:  opts.ValueFiles,
				Values:       v.Map(),
				Region:       region,
			}

			// Cluster plumbing, all best-effort: each nil leaves the checks that
			// need it to WARN about the skip.
			getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
			if cs, err := kube.NewClientset(getter); err == nil {
				in.Clientset = cs
			}
			if rc, err := kube.NewRESTConfig(getter); err == nil {
				in.RESTConfig = rc
			}
			if hc, err := kube.NewActionConfig(getter, opts.Namespace); err == nil {
				in.Helm = hc
			}

			// The chart, likewise: a failure here means the render check WARNs.
			if chrt, err := install.LocateChart(chartRef, repoURL, chartVersion); err == nil {
				in.Chart = chrt
			}

			checks := []preflight.Check{
				preflight.ValuesChecks,
				preflight.LicenseCheck,
				preflight.StaticRenderCheck,
				preflight.Analyzers,
				preflight.DBReachChecks,
				preflight.OIDCDiscoveryCheck,
				// This command creates nothing, so Input.BucketsToCreate stays empty
				// and EVERY configured bucket is probed. That is the right reading
				// for a readiness check: it answers "do the buckets these values
				// point at exist right now", and the remediation names
				// `install --create-buckets` for the ones meant to be created.
				preflight.BucketExistenceCheck,
			}

			if checkAWS {
				in.EKSClusterName = install.DeriveEKSClusterName(clusterName, opts.Kubeconfig, opts.Context)
				checks = append(checks, preflight.AWSSGCheck)
			}
			checks = append(checks, permissionsCheck(region, opts.Namespace, v))

			// The collector's progress channel is drained for the life of the
			// process and never closed — troubleshoot collector goroutines can
			// publish after their collection has been abandoned, and a send on a
			// closed channel panics. A one-shot command has no teardown to do, so
			// the drainer simply runs until exit.
			in.Progress = preflight.NewProgressChan()
			go preflight.DrainProgress(in.Progress, make(chan struct{}))

			rep := preflight.Run(cmd.Context(), in, checks...)

			out := cmd.OutOrStdout()
			if opts.JSON {
				if err := rep.WriteJSON(out); err != nil {
					return err
				}
			} else {
				rep.WriteText(out)
			}

			// Deliberate divergence from `permissions`, which calls os.Exit here:
			// returning the error keeps this RunE testable (os.Exit would take the
			// test binary with it) and lets cobra own the exit path. main.go prints
			// the error and exits 1, so the observable behaviour is the same.
			// ExitCode is binary — 1 when any check FAILed — so the count comes from
			// Counts() rather than from the exit code.
			if rep.ExitCode() != 0 {
				fail, _, _ := rep.Counts()
				return fmt.Errorf("preflight failed: %d failure(s)", fail)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&chartVersion, "chart-version", "", "chart version to check against (default: latest in the repo)")
	f.StringVar(&chartRef, "chart", "", "local chart path or alternate chart name (overrides the default repo pull; useful offline)")
	f.StringVar(&repoURL, "repo", install.DefaultRepoURL, "Helm repository URL to pull the chart from")
	f.BoolVar(&checkAWS, "aws", false, "additionally cross-check that the EKS cluster's security group can reach each managed database (read-only AWS describes)")
	f.StringVar(&clusterName, "cluster", "", "AWS EKS cluster name for the --aws security-group cross-check (default: derived from the kube --context/kubeconfig when it names an EKS cluster ARN)")
	f.StringVar(&region, "region", "", "AWS region for the --aws checks and the object-storage existence probe (default: AWS_REGION env, then us-east-1)")

	return cmd
}

// permissionsCheck adapts the AWS IAM permission probe to the Check signature.
// It builds the same infra.Plan the `permissions` command does and runs the same
// live simulation, so the two never disagree about what an install needs.
func permissionsCheck(region, namespace string, v *values.Values) preflight.Check {
	return func(ctx context.Context, _ preflight.Input) []report.Result {
		plan := infra.FromValues(v, namespace, region)
		res := infra.CheckLive(ctx, plan)
		if res == nil || res.Report == nil {
			return nil
		}
		return res.Report.Results
	}
}
