package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type policy struct {
	Tests map[string][]string `json:"tests"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("invariantcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	policyFile := flags.String("policy", "invariants/policy.json", "behavioral test manifest")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	p, err := readPolicy(*policyFile)
	if err == nil {
		err = runBehavior(p, stdout, stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "Invariant behavioral checks passed")
	return 0
}

func readPolicy(name string) (policy, error) {
	var p policy
	data, err := os.ReadFile(name)
	if err != nil {
		return p, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, err
	}
	if len(p.Tests) == 0 {
		return p, errors.New("invariant policy must include behavioral tests")
	}
	for pkg, names := range p.Tests {
		if !strings.HasPrefix(pkg, "./") || len(names) == 0 {
			return p, fmt.Errorf("invalid behavioral test group %q", pkg)
		}
		for _, name := range names {
			if !strings.HasPrefix(name, "Test") {
				return p, fmt.Errorf("invalid test name %q", name)
			}
		}
	}
	return p, nil
}

func runBehavior(p policy, stdout, stderr io.Writer) error {
	for pkg, names := range p.Tests {
		patterns := make([]string, len(names))
		for i, name := range names {
			patterns[i] = regexp.QuoteMeta(name)
		}
		cmd := exec.CommandContext(context.Background(), "go", "test", "-json", "-count=1")
		// Names are regexp-escaped and packages must start with ./; neither can
		// introduce a Go flag or shell syntax. Keep the executable argv fixed.
		cmd.Args = append(cmd.Args, "-run", "^("+strings.Join(patterns, "|")+")$", pkg)
		cmd.Env = withoutEnv(os.Environ(), "DETENT_API_TOKEN")
		data, err := cmd.Output()
		if err != nil {
			fmt.Fprintln(stderr, string(data))
			return fmt.Errorf("invariant behavior %s: %w", pkg, err)
		}
		if err := verifyTestEvents(data, names); err != nil {
			return fmt.Errorf("invariant behavior %s: %w", pkg, err)
		}
		fmt.Fprintln(stdout, "Verified", pkg, strings.Join(names, ", "))
	}
	return nil
}

func withoutEnv(env []string, key string) []string {
	var result []string
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return result
}

func verifyTestEvents(data []byte, names []string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	passed := make(map[string]bool)
	for {
		var event struct{ Action, Test string }
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if event.Action == "skip" || event.Action == "fail" {
			return fmt.Errorf("behavioral test %q reported %s", event.Test, event.Action)
		}
		if event.Action == "pass" && event.Test != "" {
			passed[event.Test] = true
		}
	}
	for _, name := range names {
		if !passed[name] {
			return fmt.Errorf("required behavioral test %q did not pass", name)
		}
	}
	return nil
}
