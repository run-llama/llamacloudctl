package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd returns the `version` subcommand, which prints the CLI version
// injected at build time via -ldflags. cobra's built-in --version flag alone is
// insufficient — the ticket requires an explicit `llamacloudctl version` command.
//
// The version string goes to cmd.OutOrStdout() rather than cmd.Println: cobra's
// Print family writes to OutOrStderr, and the root command never calls SetOut,
// so `llamacloudctl version > v.txt` would capture an empty file. Same fix as
// the terraform command.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the llamacloudctl version.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), VersionString())
			return nil
		},
	}
}
