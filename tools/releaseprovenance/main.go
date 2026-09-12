package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("releaseprovenance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repository := flags.String("repository", "", "expected owner/repository")
	tag := flags.String("tag", "", "expected release tag")
	commit := flags.String("commit", "", "expected full release commit")
	defaultBranchRef := flags.String("default-branch-ref", "", "full ref for the authenticated default branch")
	tagMessagePath := flags.String("tag-message", "", "path containing the annotated tag message")
	checkRunsPath := flags.String("github-check-runs", "", "authenticated GitHub check-runs response")
	statusesPath := flags.String("github-statuses", "", "authenticated GitHub combined-status response")
	rulesetsPath := flags.String("github-rulesets", "", "authenticated active branch rulesets as JSON values")
	outputPath := flags.String("output", "", "path for canonical release provenance JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*tagMessagePath) == "" {
		return errors.New("-tag-message is required")
	}
	if strings.TrimSpace(*outputPath) == "" {
		return errors.New("-output is required")
	}
	if strings.TrimSpace(*checkRunsPath) == "" || strings.TrimSpace(*statusesPath) == "" || strings.TrimSpace(*rulesetsPath) == "" {
		return errors.New("-github-check-runs, -github-statuses, and -github-rulesets are required")
	}
	raw, err := os.ReadFile(*tagMessagePath)
	if err != nil {
		return fmt.Errorf("read release tag message: %w", err)
	}
	manifest, err := provenance.FromTagMessage(string(raw), *repository, *tag, *commit)
	if err != nil {
		return fmt.Errorf("verify release tag provenance: %w", err)
	}
	if strings.TrimSpace(*defaultBranchRef) == "" {
		return errors.New("-default-branch-ref is required")
	}
	evidence, err := loadGitHubEvidence(*checkRunsPath, *statusesPath, *rulesetsPath, *defaultBranchRef)
	if err != nil {
		return err
	}
	if err := provenance.VerifyGitHubEvidence(manifest, evidence); err != nil {
		return fmt.Errorf("verify authenticated GitHub release evidence: %w", err)
	}
	encoded, err := provenance.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*outputPath), 0o755); err != nil {
		return fmt.Errorf("create release provenance directory: %w", err)
	}
	if err := os.WriteFile(*outputPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write release provenance: %w", err)
	}
	return nil
}

type checkRunsResponse struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		App        struct {
			ID int64 `json:"id"`
		} `json:"app"`
	} `json:"check_runs"`
}

type statusesResponse struct {
	SHA      string `json:"sha"`
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
	TotalCount int `json:"total_count"`
}

type rulesetResponse struct {
	Enforcement string `json:"enforcement"`
	Target      string `json:"target"`
	Conditions  struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context       string `json:"context"`
				IntegrationID int64  `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	} `json:"rules"`
}

func loadGitHubEvidence(checkRunsPath string, statusesPath string, rulesetsPath string, defaultBranchRef string) (provenance.GitHubEvidence, error) {
	var runs checkRunsResponse
	if err := readJSONFile(checkRunsPath, &runs); err != nil {
		return provenance.GitHubEvidence{}, fmt.Errorf("read authenticated GitHub check runs: %w", err)
	}
	if runs.TotalCount != len(runs.CheckRuns) {
		return provenance.GitHubEvidence{}, fmt.Errorf("authenticated GitHub check-run evidence is truncated: total %d, received %d", runs.TotalCount, len(runs.CheckRuns))
	}
	var statuses statusesResponse
	if err := readJSONFile(statusesPath, &statuses); err != nil {
		return provenance.GitHubEvidence{}, fmt.Errorf("read authenticated GitHub statuses: %w", err)
	}
	if statuses.TotalCount != len(statuses.Statuses) {
		return provenance.GitHubEvidence{}, fmt.Errorf("authenticated GitHub status evidence is truncated: total %d, received %d", statuses.TotalCount, len(statuses.Statuses))
	}

	evidence := provenance.GitHubEvidence{}
	for _, run := range runs.CheckRuns {
		evidence.Checks = append(evidence.Checks, provenance.ObservedCheck{
			Name:          run.Name,
			Status:        run.Status,
			Conclusion:    run.Conclusion,
			Commit:        run.HeadSHA,
			CheckRunID:    run.ID,
			IntegrationID: run.App.ID,
		})
	}
	for _, status := range statuses.Statuses {
		observed := provenance.ObservedCheck{Name: status.Context, Commit: statuses.SHA}
		if strings.EqualFold(strings.TrimSpace(status.State), "pending") {
			observed.Status = "in_progress"
		} else {
			observed.Status = "completed"
			observed.Conclusion = status.State
		}
		evidence.Checks = append(evidence.Checks, observed)
	}

	rawRulesets, err := os.ReadFile(rulesetsPath)
	if err != nil {
		return provenance.GitHubEvidence{}, fmt.Errorf("read authenticated GitHub rulesets: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(rawRulesets))
	for {
		var ruleset rulesetResponse
		if err := decoder.Decode(&ruleset); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return provenance.GitHubEvidence{}, fmt.Errorf("decode authenticated GitHub rulesets: %w", err)
		}
		if !strings.EqualFold(ruleset.Enforcement, "active") || !strings.EqualFold(ruleset.Target, "branch") || !rulesetAppliesToDefaultBranch(ruleset, defaultBranchRef) {
			continue
		}
		for _, rule := range ruleset.Rules {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, required := range rule.Parameters.RequiredStatusChecks {
				evidence.RequiredChecks = append(evidence.RequiredChecks, provenance.RepositoryRequirement{
					Name:          required.Context,
					IntegrationID: required.IntegrationID,
				})
			}
		}
	}
	return evidence, nil
}

func readJSONFile(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func rulesetAppliesToDefaultBranch(ruleset rulesetResponse, defaultBranchRef string) bool {
	for _, pattern := range ruleset.Conditions.RefName.Exclude {
		if refPatternMatches(pattern, defaultBranchRef) {
			return false
		}
	}
	if len(ruleset.Conditions.RefName.Include) == 0 {
		return true
	}
	for _, pattern := range ruleset.Conditions.RefName.Include {
		if refPatternMatches(pattern, defaultBranchRef) {
			return true
		}
	}
	return false
}

func refPatternMatches(pattern string, defaultBranchRef string) bool {
	pattern = strings.TrimSpace(pattern)
	switch pattern {
	case "~ALL", "~DEFAULT_BRANCH":
		return true
	}
	matched, err := path.Match(pattern, strings.TrimSpace(defaultBranchRef))
	return err == nil && matched
}
