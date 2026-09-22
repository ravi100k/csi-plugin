// generate-operands must run from operator/, as the Makefile and Dockerfile do.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/hammer-space/csi-plugin/operator/internal/manifests"
)

func main() {
	source := flag.String("source", "../deploy/kubernetes/kubernetes-1.36/plugin.yaml", "canonical CSI manifest")
	output := flag.String("output", "internal/operator/operands.json", "generated embedded template")
	check := flag.Bool("check", false, "fail if the embedded template is stale, without changing files")
	flag.Parse()
	if err := run(*source, *output, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(source, output string, check bool) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := manifests.Generate(f)
	if err != nil {
		return err
	}
	previous, err := os.ReadFile(output)
	if err == nil && bytes.Equal(previous, data) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is stale; run make -C operator generate from the repository root", output)
	}
	return os.WriteFile(output, data, 0644)
}
