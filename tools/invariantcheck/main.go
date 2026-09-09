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
	Checks    []string            `json:"checks"`
	Protected []string            `json:"protected"`
	Tests     map[string][]string `json:"tests"`
}

type check struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("invariantcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mode := flags.String("mode", "behavior", "behavior, protect, release, or running")
	policyFile := flags.String("policy", "invariants/policy.json", "trusted policy file")
	base := flags.String("base", "", "trusted base commit for protection")
	root := flags.String("root", ".", "repository containing base and proposed objects")
	head := flags.String("head", "", "proposed or released full commit SHA")
	repository := flags.String("repo", "digitaldrywood/detent", "GitHub repository")
	evidence := flags.String("evidence", "", "running /api/v1/state JSON file")
	version := flags.String("version", "", "expected running version")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	p, err := readPolicy(*policyFile)
	if err == nil {
		switch *mode {
		case "behavior":
			err = runBehavior(p, stdout, stderr)
		case "protect":
			if !fullSHA(*base) || !fullSHA(*head) {
				err = errors.New("protect requires full base and head commit SHAs")
				break
			}
			var diff []byte
			diff, err = command("git", "-C", *root, "diff", "--no-ext-diff", "--name-only", "--no-renames", "-z", *base, *head, "--")
			if err == nil {
				err = protect(p, strings.Split(string(diff), "\x00"))
			}
		case "release":
			err = releaseEvidence(p, *repository, *head)
		case "running":
			var data []byte
			data, err = os.ReadFile(*evidence)
			if err == nil {
				err = runningEvidence(data, *version, *head)
			}
		default:
			err = fmt.Errorf("unknown mode %q", *mode)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "Invariant evidence passed:", *mode)
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
	if len(p.Checks) == 0 || len(p.Protected) == 0 || len(p.Tests) == 0 {
		return p, errors.New("invariant policy must include checks, protected paths, and behavioral tests")
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

func fullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func protect(p policy, files []string) error {
	var failures []error
	for _, file := range files {
		for _, protected := range p.Protected {
			if file == protected || strings.HasSuffix(protected, "/") && strings.HasPrefix(file, protected) {
				failures = append(failures, fmt.Errorf("protected change %s requires Cory's independently authenticated approval; agent exemptions are not accepted", file))
				break
			}
		}
	}
	return errors.Join(failures...)
}

func command(name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Env = withoutEnv(os.Environ(), "DETENT_API_TOKEN")
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("%s failed: %w", name, err)
	}
	return output, nil
}

func releaseEvidence(p policy, repository, sha string) error {
	if !fullSHA(sha) {
		return errors.New("release requires a full commit SHA")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) {
		return errors.New("invalid repository")
	}
	data, err := command("gh", "api", "--paginate", "--jq", ".check_runs", "repos/"+repository+"/commits/"+sha+"/check-runs?filter=latest&per_page=100")
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var checks []check
	for {
		var page []check
		if err := decoder.Decode(&page); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		checks = append(checks, page...)
	}
	return verifyChecks(p.Checks, sha, checks)
}

func verifyChecks(required []string, sha string, checks []check) error {
	if !fullSHA(sha) || len(required) == 0 {
		return errors.New("missing exact commit or required checks")
	}
	var failures []error
	for _, name := range required {
		found := false
		for _, c := range checks {
			if c.Name != name {
				continue
			}
			found = true
			if c.HeadSHA != sha || c.App.ID != 15368 || c.Status != "completed" || c.Conclusion != "success" {
				failures = append(failures, fmt.Errorf("required check %q lacks successful GitHub Actions evidence for %s", name, sha))
			}
		}
		if !found {
			failures = append(failures, fmt.Errorf("required check %q is missing for %s", name, sha))
		}
	}
	return errors.Join(failures...)
}

func runningEvidence(data []byte, version, sha string) error {
	var state struct {
		Instance struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if !fullSHA(sha) || version == "" || state.Instance.Version != version || state.Instance.Commit != sha {
		return fmt.Errorf("running artifact does not match version %q and full tested commit %q", version, sha)
	}
	return nil
}

func runBehavior(p policy, stdout, stderr io.Writer) error {
	for pkg, names := range p.Tests {
		patterns := make([]string, len(names))
		for i, name := range names {
			patterns[i] = regexp.QuoteMeta(name)
		}
		data, err := command("go", "test", "-json", "-count=1", "-run", "^("+strings.Join(patterns, "|")+")$", pkg)
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
