// Command llamacloudctl manages a customer-managed LlamaCloud (LlamaParse)
// Kubernetes deployment built on the llamacloud Helm chart.
package main

import (
	"fmt"
	"os"

	"github.com/run-llama/llamacloudctl/internal/cli"
)

func main() {
	root, _ := cli.NewRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
