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
	destination, err := reportingDestination(ctx, os.Getenv)
	if err == nil {
		err = reportTo(ctx, os.Stdin, runGH, os.Getenv, destination)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runGH(ctx context.Context, input string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh")
	cmd.Args = append(cmd.Args, args...)
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

type issueDestination interface {
	load(context.Context) error
	file(context.Context, string, string, string, string, []string, bool) error
	success(context.Context, func(string) string) error
}

type githubDestination struct {
	command    ghCommand
	repository string
	issues     map[string]int
}

func (g *githubDestination) load(ctx context.Context) error {
	owner, name, _ := strings.Cut(g.repository, "/")
	output, err := g.command(ctx, "", "api", "graphql", "--paginate", "-f", "query="+openIssuesQuery,
		"-f", "owner="+owner, "-f", "name="+name, "--jq", ".data.repository.issues.nodes[]")
	if err != nil {
		return fmt.Errorf("list open machine issues: %w", err)
	}
	g.issues = map[string]int{}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var issue openIssue
		if err := decoder.Decode(&issue); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode open issues: %w", err)
		}
		if origin, ok := issueorigin.Parse(issue.Body); ok {
			g.issues[origin.Fingerprint] = issue.Number
		}
	}

	return nil
}

func (g *githubDestination) file(ctx context.Context, fingerprint, summary, body, _ string, labels []string, _ bool) error {
	return fileProblem(ctx, g.command, g.repository, g.issues, fingerprint, summary, body, labels)
}

func (*githubDestination) success(context.Context, func(string) string) error { return nil }

func report(ctx context.Context, input io.Reader, gh ghCommand, getenv func(string) string) error {
	return reportTo(ctx, input, gh, getenv, &githubDestination{command: gh, repository: getenv("GITHUB_REPOSITORY")})
}

func reportTo(ctx context.Context, input io.Reader, gh ghCommand, getenv func(string) string, destination issueDestination) error {
	var jobs []job
	if err := json.NewDecoder(input).Decode(&jobs); err != nil {
		return fmt.Errorf("decode scheduled CI jobs: %w", err)
	}
	repository := getenv("GITHUB_REPOSITORY")
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" {
		return errors.New("GITHUB_REPOSITORY must be owner/repository")
	}
	if err := destination.load(ctx); err != nil {
		return err
	}

	runURL := fmt.Sprintf("%s/%s/actions/runs/%s/attempts/%s", getenv("GITHUB_SERVER_URL"), repository, getenv("GITHUB_RUN_ID"), getenv("GITHUB_RUN_ATTEMPT"))
	for _, j := range jobs {
		if (j.Name == "Finalize scheduled validation" && j.Conclusion != "failure") || j.Conclusion == "success" {
			continue
		}
		log, logErr := gh(ctx, "", "api", fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repository, j.ID), "--allow-escape-sequences")
		problems := parseProblems(string(log), getenv("GITHUB_WORKSPACE"))
		labels := []string{"detent:todo", "hotfix", "ci-scheduled-failure"}
		if len(problems) == 0 {
			labels = []string{"detent:backlog", "ci-scheduled-failure"}
			evidence := "No reliable failed test or source diagnostic was available; this identity is limited to the job."
			if excerpt := strings.TrimSpace(ansiEscape.ReplaceAllString(string(log), "")); excerpt != "" {
				evidence += "\n\n" + diagnosticExcerpt(excerpt)
			}
			problems = []problem{{Key: "scheduled-ci:" + repository + ":" + j.Name, Summary: "scheduled " + j.Name + " failure", Evidence: evidence}}
		}
		for _, p := range problems {
			sourceFailure := !strings.HasPrefix(p.Key, "scheduled-ci:")
			fingerprint := issueorigin.Fingerprint(p.Key)
			if strings.HasPrefix(p.Key, "scheduled-ci:") {
				fingerprint = legacyJobFingerprint(p.Key)
			}
			disposition := "Let the next scheduled full validation confirm the repair; a green run closes scheduled repair issues."
			if _, native := destination.(*cloudDestination); native {
				disposition = "Let the next scheduled full validation confirm the repair. Scheduled evidence does not authorize review approval or native completion."
				if sourceFailure {
					disposition += " Under the human-approved Detent scheduled reporting policy, newly reported proven source or test blockers enter Todo at High priority. Reused work retains its lane, human questions and operator holds, with at least High priority and Urgent preserved. Verify this reported failure on the worker's current base using focused diagnostics; do not require a local-gate status or wait for CI before ordinary issue merging. This pinned failure does not prove a current staging outage or that the current head still fails."
				}
			}
			body := fmt.Sprintf("Scheduled validation job **%s** (%s) failed on development commit %s.\n\nRun: %s\nJob: %s\nProblem: `%s`\n\n```text\n%s\n```\n\nDiagnose this problem using the linked logs. Runner setup, backend startup, network/download, and protocol failures belong to the CI instance. %s\n\n```detent-agent\nschema: 1\neffort: high\n```", j.Name, j.Conclusion, getenv("CI_DEVELOP_SHA"), runURL, j.URL, p.Key, p.Evidence, disposition)
			if strings.HasPrefix(p.Key, "scheduled-ci:") {
				body += "\n\nThis issue is an intake for the CI instance owner. No source repair is authorized until a reproducible test or source diagnostic is identified."
			}
			if logErr != nil {
				if _, native := destination.(*cloudDestination); native {
					body += "\n\nJob logs could not be read; consult the original Actions job evidence."
				} else {
					body += "\n\nJob logs could not be read: " + logErr.Error()
				}
			}
			body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: runURL, Fingerprint: fingerprint})
			if err := destination.file(ctx, fingerprint, p.Summary, body, occurrenceKey(getenv, j, fingerprint), labels, sourceFailure); err != nil {
				return fmt.Errorf("report %s (%s): %w", j.Name, p.Key, err)
			}
		}
	}
	if allGreen(jobs) {
		return destination.success(ctx, getenv)
	}
	return nil
}

func fileProblem(ctx context.Context, gh ghCommand, repository string, issues map[string]int, fingerprint, summary, body string, labels []string) error {
	path := "repos/" + repository + "/issues"
	title := []rune("fix(ci): " + summary)
	if len(title) > 256 {
		title = append(title[:255], '…')
	}
	payload := map[string]any{"title": string(title), "body": body, "labels": labels}
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
