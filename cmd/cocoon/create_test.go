package main

import (
	"flag"
	"testing"

	"github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
)

func testCreateCLIContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	for _, f := range vmCreateFlags() {
		if err := f.Apply(fs); err != nil {
			t.Fatalf("apply flag: %v", err)
		}
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return cli.NewContext(cli.NewApp(), fs, nil)
}

func TestParseCreateOptions_AutoStrategyByDefault(t *testing.T) {
	t.Parallel()
	c := testCreateCLIContext(t, "docker.io/library/ubuntu:24.04")

	opts, err := parseCreateOptions(c, "create")
	if err != nil {
		t.Fatalf("parseCreateOptions: %v", err)
	}
	if opts.BootStrategy != "" {
		t.Fatalf("BootStrategy = %q, want empty (auto)", opts.BootStrategy)
	}
}

func TestParseCreateOptions_OCIDebugOverride(t *testing.T) {
	t.Parallel()
	c := testCreateCLIContext(t, "--oci", "docker.io/library/ubuntu:24.04")

	opts, err := parseCreateOptions(c, "create")
	if err != nil {
		t.Fatalf("parseCreateOptions: %v", err)
	}
	if opts.BootStrategy != types.BootStrategyDirect {
		t.Fatalf("BootStrategy = %q, want %q", opts.BootStrategy, types.BootStrategyDirect)
	}
}
