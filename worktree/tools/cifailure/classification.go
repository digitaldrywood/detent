package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/progresscheck"
)

var ciExitCode = regexp.MustCompile(`Process completed with exit code ([0-9]+)\.`)

func changedFiles(ctx context.Context, gh ghCommand, getenv func(string) string) ([]string, string) {
	output, err := gh(ctx, "", "api", fmt.Sprintf("repos/%s/commits/%s?per_page=100", getenv("GITHUB_REPOSITORY"), getenv("CI_DEVELOP_SHA")), "--jq", "[.files[:100][].filename]")
	var files []string
	if err != nil || json.Unmarshal(output, &files) != nil {
		return nil, "Changed files unavailable."
	}
	return files, "Changed files are limited to the first 100 paths of the tested commit."
}

func boundedCIText(value string, limit int) string {
	if len(value) > limit {
		value = value[len(value)-limit:]
	}
	return strings.ToValidUTF8(value, "")
}

func classifyFailure(ctx context.Context, destination issueDestination, j job, log []byte, logErr error, files []string, missing string, getenv func(string) string) (progresscheck.CIDecision, error) {
	request := progresscheck.CIFailure{Repository: getenv("GITHUB_REPOSITORY"), HeadSHA: getenv("CI_DEVELOP_SHA"), RunID: getenv("GITHUB_RUN_ID"), RunAttempt: getenv("GITHUB_RUN_ATTEMPT"),
		JobID: strconv.FormatInt(j.ID, 10), JobName: boundedCIText(j.Name, 512), ExitStatus: j.Conclusion, LogTail: boundedCIText(ansiEscape.ReplaceAllString(string(log), ""), 8000), MissingEvidence: missing}
	if request.ExitStatus == "" {
		request.ExitStatus = "unknown"
	}
	for _, step := range j.Steps {
		if step.Conclusion != "success" && step.Conclusion != "skipped" {
			request.Step = boundedCIText(step.Name, 512)
			request.ExitCode = step.ExitCode
			break
		}
	}
	if request.ExitCode == nil {
		matches := ciExitCode.FindAllStringSubmatch(request.LogTail, -1)
		if len(matches) > 0 {
			if code, err := strconv.Atoi(matches[len(matches)-1][1]); err == nil {
				request.ExitCode = &code
			}
		}
	}
	if request.Step == "" {
		request.MissingEvidence += " Failing step unavailable."
	}
	if request.ExitCode == nil {
		request.MissingEvidence += " Numeric exit code unavailable; job conclusion supplied as exit status."
	}
	if logErr != nil {
		request.MissingEvidence += " Job logs unavailable."
	}
	for i, file := range files {
		if i == 100 {
			break
		}
		request.ChangedFiles = append(request.ChangedFiles, boundedCIText(file, 256))
	}
	for {
		encoded, err := json.Marshal(request)
		if err != nil {
			return progresscheck.CIDecision{}, fmt.Errorf("encode CI classification evidence: %w", err)
		}
		if len(encoded) <= progresscheck.MaxInputBytes {
			break
		}
		if len(request.ChangedFiles) > 0 {
			request.ChangedFiles = request.ChangedFiles[:len(request.ChangedFiles)-1]
		} else if len(request.LogTail) > 0 {
			request.LogTail = boundedCIText(request.LogTail, len(request.LogTail)/2)
		} else {
			return progresscheck.CIDecision{}, errors.New("CI failure identity exceeds byte bound")
		}
	}
	if err := request.Validate(); err != nil {
		return progresscheck.CIDecision{}, err
	}
	decision := progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "No native AI decision connection is configured for this destination; diagnose the retained evidence through the source repair path."}
	if cloud, ok := destination.(*cloudDestination); ok {
		if err := cloud.command(ctx, "classify_ci_failure", map[string]any{"project_id": cloud.project, "failure": request}, &decision); err != nil {
			return progresscheck.CIDecision{}, fmt.Errorf("classify scheduled failure: %w", err)
		}
	}
	return decision, decision.Validate()
}
