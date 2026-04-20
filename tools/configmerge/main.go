// Package main merges NetSonar YAML configs for local lab workflows.
package main

import (
	"flag"
	"fmt"
	"os"

	"go.yaml.in/yaml/v4"

	"netsonar/internal/config"
)

func main() {
	basePath := flag.String("base", "", "Base NetSonar config path")
	overlayPath := flag.String("overlay", "", "Overlay NetSonar config path")
	outPath := flag.String("out", "", "Output merged config path")
	flag.Parse()

	if *basePath == "" || *overlayPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: configmerge --base base.yaml --overlay overlay.yaml --out merged.yaml")
		os.Exit(2)
	}

	base, err := config.LoadConfig(*basePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load base config: %v\n", err)
		os.Exit(1)
	}

	overlay, err := config.LoadConfig(*overlayPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load overlay config: %v\n", err)
		os.Exit(1)
	}

	merged := *base
	merged.Targets = append(append([]config.TargetConfig{}, base.Targets...), overlay.Targets...)

	data, err := yaml.Marshal(&merged)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal merged config: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(*outPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write merged config: %v\n", err)
		os.Exit(1)
	}

	if _, err := config.LoadConfig(*outPath); err != nil {
		fmt.Fprintf(os.Stderr, "validate merged config: %v\n", err)
		os.Exit(1)
	}
}
