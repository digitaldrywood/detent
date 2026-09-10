// Command cifailure reports failed post-merge CI jobs through machine intake.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

type job struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
}

type issueCreator interface {
	CreateIntakeIssue(context.Context, intake.IssueDraft) (intake.Issue, error)
}

func main() {
	if err := run(os.Stdin, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(input io.Reader, getenv func(string) string) error {
	backend, err := github.NewConnector(github.Config{
		APIKey:             getenv("GH_TOKEN"),
		Repository:         getenv("GITHUB_REPOSITORY"),
		GitHubStatusSource: github.GitHubStatusSourceLabel,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return report(ctx, input, backend, getenv("GITHUB_SHA"))
}

func report(ctx context.Context, input io.Reader, backend issueCreator, sha string) error {
	decoder := json.NewDecoder(input)
	var failures []error
	for {
		var j job
		if err := decoder.Decode(&j); errors.Is(err, io.EOF) {
			return errors.Join(failures...)
		} else if err != nil {
			return errors.Join(append(failures, fmt.Errorf("decode CI jobs: %w", err))...)
		}
		if !integrationJob(j.Name) || (j.Conclusion != "failure" && j.Conclusion != "timed_out") {
			continue
		}
		body := fmt.Sprintf("Post-merge integration job **%s** failed on main.\n\nJob: %s\nCommit: %s\n\nThis tracks CI instance health; it does not attribute a defect to the merged issue or change. Diagnose the linked logs before proposing a fix. Runner setup, backend startup, network/download, and protocol failures belong to the CI instance. Propose product changes only when the logs establish a product defect.\n\n```detent-agent\nschema: 1\neffort: low\n```", j.Name, j.URL, sha)
		body = issueorigin.Stamp(body, issueorigin.Origin{
			Kind: "doctor", Instance: "github-actions", Source: j.URL,
			Fingerprint: issueorigin.Fingerprint("ci integration job " + j.Name),
		})
		if _, err := backend.CreateIntakeIssue(ctx, intake.IssueDraft{
			Title: "ci: " + j.Name + " failed on main", Body: body,
		}); err != nil {
			failures = append(failures, fmt.Errorf("report %s: %w", j.Name, err))
		}
	}
}

func integrationJob(name string) bool {
	switch name {
	case "Portability Verify (macos-latest)", "Portability Verify (windows-latest)",
		"Windows Core", "Installer Smoke (ubuntu-latest)", "Installer Smoke (windows-latest)",
		"GoReleaser Snapshot":
		return true
	default:
		return false
	}
}
