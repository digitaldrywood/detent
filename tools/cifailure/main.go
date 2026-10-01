// Command cifailure reports scheduled validation problems through machine intake.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
)

type job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
}

type openIssue struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

type ghCommand func(context.Context, string, ...string) ([]byte, error)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := report(ctx, os.Stdin, runGH, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runGH(ctx context.Context, input string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...) // #nosec G204 -- gh is fixed; API arguments are assembled by this package and passed directly without a shell.
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return output, nil
}

const openIssuesQuery = `query($owner:String!,$name:String!,$endCursor:String) {
  repository(owner:$owner,name:$name) {
    issues(first:100,after:$endCursor,states:OPEN) {
      nodes { number body }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

func report(ctx context.Context, input io.Reader, gh ghCommand, getenv func(string) string) error {
	var jobs []job
	if err := json.NewDecoder(input).Decode(&jobs); err != nil {
		return fmt.Errorf("decode scheduled CI jobs: %w", err)
	}
	repository := getenv("GITHUB_REPOSITORY")
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" {
		return errors.New("GITHUB_REPOSITORY must be owner/repository")
	}
	// List all open issues, without a label filter or search-index delay. Keep
	// newly filed issues in this snapshot so later jobs attach occurrences too.
	output, err := gh(ctx, "", "api", "graphql", "--paginate", "-f", "query="+openIssuesQuery,
		"-f", "owner="+owner, "-f", "name="+name, "--jq", ".data.repository.issues.nodes[]")
	if err != nil {
		return fmt.Errorf("list open machine issues: %w", err)
	}
	issues := map[string]int{}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var issue openIssue
		if err := decoder.Decode(&issue); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode open issues: %w", err)
		}
		if origin, ok := issueorigin.Parse(issue.Body); ok {
			issues[origin.Fingerprint] = issue.Number
		}
	}

	runURL := fmt.Sprintf("%s/%s/actions/runs/%s/attempts/%s", getenv("GITHUB_SERVER_URL"), repository, getenv("GITHUB_RUN_ID"), getenv("GITHUB_RUN_ATTEMPT"))
	var failures []error
	for _, j := range jobs {
		if (j.Name == "Finalize scheduled validation" && j.Conclusion != "failure") || j.Conclusion == "success" {
			continue
		}
		log, logErr := gh(ctx, "", "api", fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repository, j.ID))
		problems := parseProblems(string(log), getenv("GITHUB_WORKSPACE"))
		if len(problems) == 0 {
			problems = []problem{{Key: "scheduled-ci:" + repository + ":" + j.Name, Summary: "scheduled " + j.Name + " failure", Evidence: "No reliable failed test or source diagnostic was available; this identity is limited to the job."}}
		}
		for _, p := range problems {
			// Preserve the legacy job fingerprint for conservative fallbacks.
			fingerprint := issueorigin.Fingerprint(p.Key)
			if strings.HasPrefix(p.Key, "scheduled-ci:") {
				fingerprint = legacyJobFingerprint(p.Key)
			}
			body := fmt.Sprintf("Scheduled validation job **%s** (%s) failed on development commit %s.\n\nRun: %s\nJob: %s\nProblem: `%s`\n\n```text\n%s\n```\n\nDiagnose this problem using the linked logs. Runner setup, backend startup, network/download, and protocol failures belong to the CI instance. Let the next scheduled full validation confirm the repair; a green run closes scheduled repair issues.\n\n```detent-agent\nschema: 1\neffort: high\n```", j.Name, j.Conclusion, getenv("CI_DEVELOP_SHA"), runURL, j.URL, p.Key, p.Evidence)
			if logErr != nil {
				body += "\n\nJob logs could not be read: " + logErr.Error()
			}
			body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: runURL, Fingerprint: fingerprint})
			if err := fileProblem(ctx, gh, repository, issues, fingerprint, p.Summary, body); err != nil {
				failures = append(failures, fmt.Errorf("report %s (%s): %w", j.Name, p.Key, err))
			}
		}
	}
	return errors.Join(failures...)
}

func fileProblem(ctx context.Context, gh ghCommand, repository string, issues map[string]int, fingerprint, summary, body string) error {
	path := "repos/" + repository + "/issues"
	title := []rune("fix(ci): " + summary)
	if len(title) > 256 {
		title = append(title[:255], '…')
	}
	payload := map[string]any{"title": string(title), "body": body, "labels": []string{"detent:todo", "hotfix", "ci-scheduled-failure"}}
	if number := issues[fingerprint]; number != 0 {
		path += "/" + strconv.Itoa(number) + "/comments"
		payload = map[string]any{"body": issueorigin.Occurrence(body)}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	output, err := gh(ctx, string(encoded), "api", "--method", "POST", path, "--input", "-")
	if err != nil {
		return err
	}
	if issues[fingerprint] != 0 {
		return nil
	}
	var created openIssue
	if err := json.Unmarshal(output, &created); err != nil {
		return fmt.Errorf("decode created issue: %w", err)
	}
	if created.Number <= 0 {
		return errors.New("created issue has no number")
	}
	issues[fingerprint] = created.Number
	return nil
}
