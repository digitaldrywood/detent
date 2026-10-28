package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const scheduledCloudProject = issueorigin.DetentCloudProjectID

const scheduledListLimit = 100

type cloudCommand func(context.Context, string, map[string]any, any) error

type cloudIssue struct {
	item   tracker.NativeIssue
	bodies []string
}

type cloudDestination struct {
	command       cloudCommand
	project       string
	organization  string
	issues        []*cloudIssue
	evidence      io.Writer
	scannedAll    bool
	previousGreen map[string]string
}

func reportingDestination(_ context.Context, getenv func(string) string) (issueDestination, error) {
	if getenv("GITHUB_REPOSITORY") != "digitaldrywood/detent" {
		return &githubDestination{command: runGH, repository: getenv("GITHUB_REPOSITORY")}, nil
	}
	destination, err := cloudReportingDestination(getenv, &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	if err != nil {
		return nil, err
	}
	return destination, nil
}

func cloudReportingDestination(getenv func(string) string, client *http.Client) (*cloudDestination, error) {
	project, token := getenv("DETENT_PROJECT_ID"), getenv("DETENT_API_KEY")
	if project != scheduledCloudProject || strings.TrimSpace(token) == "" {
		return nil, errors.New("scheduled Cloud reporting requires its selected project and scoped API key")
	}
	if getenv("GITHUB_RUN_ID") == "" || getenv("GITHUB_RUN_ATTEMPT") == "" {
		return nil, errors.New("scheduled Cloud reporting requires run and attempt identity")
	}
	transport, err := newCloudREST(getenv("DETENT_MCP_URL"), project, token, client)
	if err != nil {
		return nil, err
	}
	return &cloudDestination{command: transport.call, project: project, evidence: os.Stdout}, nil
}

func (c *cloudDestination) load(ctx context.Context) error {
	var config operatortool.WorkReadResult[struct {
		Project tracker.NativeProject `json:"project"`
	}]
	if err := c.command(ctx, "work_config", map[string]any{"project_id": c.project}, &config); err != nil {
		return err
	}
	project := config.Data.Project
	if config.ProjectID != c.project || string(project.ID) != c.project || project.Profile != "native" || project.OrganizationID == "" {
		return errors.New("scheduled destination is not the selected native project")
	}
	c.organization = string(project.OrganizationID)
	backlog, todo := false, false
	for _, state := range project.States {
		if state.Name == "Backlog" && !state.Dispatchable && !state.Terminal && !state.OperatorOnly {
			backlog = true
		}
		if state.Name == "Todo" && state.Dispatchable && !state.Terminal && !state.OperatorOnly {
			todo = true
		}
	}
	if !backlog {
		return errors.New("scheduled destination has no nondispatchable Backlog")
	}
	if !todo {
		return errors.New("scheduled destination has no dispatchable Todo")
	}
	c.issues = nil
	c.scannedAll = false
	for _, selector := range []map[string]any{{"query": "detent-origin"}, {"query": "detent-audit-fp"}, {"label": "ci-scheduled-failure"}} {
		if err := c.scan(ctx, selector); err != nil {
			return err
		}
	}
	return nil
}

func (c *cloudDestination) scan(ctx context.Context, selector map[string]any) error {
	cursor := ""
	for {
		args := map[string]any{"project_id": c.project, "open": true, "limit": scheduledListLimit, "cursor": cursor}
		for key, value := range selector {
			args[key] = value
		}
		var page operatortool.WorkReadResult[tracker.Page[tracker.NativeIssue]]
		if err := c.command(ctx, "work_list", args, &page); err != nil {
			return err
		}
		if page.ProjectID != c.project || page.Data.Items == nil {
			return errors.New("scheduled work page belongs to another project")
		}
		for _, item := range page.Data.Items {
			if string(item.ProjectID) != c.project || string(item.OrganizationID) != c.organization || item.WorkItemID == "" {
				return errors.New("scheduled work item belongs to another destination")
			}
			if ((item.Terminal || item.Archived) && selector["open"] != false) || slices.ContainsFunc(c.issues, func(loaded *cloudIssue) bool { return loaded.item.WorkItemID == item.WorkItemID }) {
				continue
			}
			var detail operatortool.WorkReadResult[tracker.NativeIssue]
			if err := c.command(ctx, "work_item", map[string]any{"project_id": c.project, "reference": string(item.WorkItemID)}, &detail); err != nil {
				return err
			}
			if detail.ProjectID != c.project || detail.Reference != string(item.WorkItemID) || detail.Data.ProjectID != item.ProjectID || detail.Data.OrganizationID != item.OrganizationID || detail.Data.WorkItemID != item.WorkItemID || slices.Contains(detail.Data.OmittedFields, "body") {
				return errors.New("scheduled work item detail is incomplete or belongs to another destination")
			}
			item = detail.Data
			issue := &cloudIssue{item: item, bodies: []string{item.Body}}
			if err := c.comments(ctx, issue); err != nil {
				return err
			}
			c.issues = append(c.issues, issue)
		}
		if page.Data.NextCursor == "" {
			return nil
		}
		if page.Data.NextCursor == cursor {
			return errors.New("scheduled work pagination did not advance")
		}
		cursor = page.Data.NextCursor
	}
}

func (c *cloudDestination) comments(ctx context.Context, issue *cloudIssue) error {
	cursor := ""
	for {
		var page operatortool.WorkReadResult[tracker.Page[tracker.NativeComment]]
		if err := c.command(ctx, "work_comments", map[string]any{"project_id": c.project, "reference": string(issue.item.WorkItemID), "limit": scheduledListLimit, "cursor": cursor}, &page); err != nil {
			return err
		}
		if page.ProjectID != c.project || page.Reference != string(issue.item.WorkItemID) || page.Data.Items == nil {
			return errors.New("scheduled comment page belongs to another item")
		}
		for _, comment := range page.Data.Items {
			if comment.WorkItemID != issue.item.WorkItemID || string(comment.ProjectID) != c.project || string(comment.OrganizationID) != c.organization {
				return errors.New("scheduled comment belongs to another destination")
			}
			issue.bodies = append(issue.bodies, comment.Body)
		}
		if page.Data.NextCursor == "" {
			return nil
		}
		if page.Data.NextCursor == cursor {
			return errors.New("scheduled comment pagination did not advance")
		}
		cursor = page.Data.NextCursor
	}
}

func occurrenceKey(getenv func(string) string, j job, fingerprint string) string {
	return issueorigin.Fingerprint(strings.Join([]string{getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID"), getenv("GITHUB_RUN_ATTEMPT"), strconv.FormatInt(j.ID, 10), j.Name, fingerprint}, ":"))
}

func occurrenceMarker(key string) string { return "<!-- detent-scheduled-occurrence:" + key + " -->" }

func (c *cloudDestination) file(ctx context.Context, fingerprint, summary, body, key string, labels []string, sourceFailure bool) error {
	if sourceFailure && stabilizationScope(body) == "" && !slices.Contains(labels, "ci-infrastructure-failure") {
		evidence := gate.ParseScheduledEvidence(body)
		if len(evidence) == 1 && c.previousGreen[evidence[0].JobName] != "" {
			var result struct {
				Attributed bool `json:"attributed"`
			}
			args := map[string]any{"project_id": c.project, "request_id": "scheduled-attribution-" + key, "body": body + "\n\n" + occurrenceMarker(key), "scheduled_failure": tracker.ScheduledFailure{PreviousGreenSHA: c.previousGreen[evidence[0].JobName], Evidence: evidence[0]}}
			if err := c.publish(ctx, "attribute_scheduled_failure", args, &result); err != nil {
				return err
			}
			if result.Attributed {
				return nil
			}
		}
	}
	if err := c.scan(ctx, map[string]any{"fingerprint": fingerprint}); err != nil {
		return err
	}
	if stabilizationScope(body) != "" {
		if err := c.scan(ctx, map[string]any{"fingerprint": fingerprint, "open": false, "archived": "all"}); err != nil {
			return err
		}
	}
	marker := occurrenceMarker(key)
	replay, match := c.match(body, marker)
	if replay == nil && match == nil && !c.scannedAll {
		if err := c.scan(ctx, nil); err != nil {
			return err
		}
		c.scannedAll = true
		replay, match = c.match(body, marker)
	}
	if replay != nil {
		if replay.item.Terminal || replay.item.Archived {
			return nil
		}
		return c.updateFailure(ctx, replay, key, labels)
	}
	body += "\n\n" + marker
	if match != nil {
		if err := c.updateFailure(ctx, match, key, labels); err != nil {
			return err
		}
		return c.comment(ctx, match, issueorigin.Occurrence(body), key)
	}
	title := "fix(ci): " + summary
	for len(title) > 256 {
		runes := []rune(title)
		title = string(runes[:len(runes)-1])
	}
	const state = "Todo"
	priority := 1
	args := map[string]any{"project_id": c.project, "request_id": "scheduled-create-" + key, "title": title, "description": body, "state": state, "labels": labels, "priority": priority + 1}
	var result struct {
		ResourceID string           `json:"resource_id"`
		Revision   tracker.Revision `json:"revision,string"`
	}
	if err := c.publish(ctx, "file_issue", args, &result); err != nil {
		return err
	}
	if result.ResourceID == "" {
		return errors.New("scheduled Cloud creation returned no native identity")
	}
	retainedBody, ok := args["description"].(string)
	if !ok {
		return errors.New("scheduled Cloud description is not a string")
	}
	c.issues = append(c.issues, &cloudIssue{item: tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(result.ResourceID), Revision: result.Revision}, State: state, Priority: &priority, Labels: labels}, bodies: []string{retainedBody}})
	return nil
}

func (c *cloudDestination) match(body, marker string) (replay, match *cloudIssue) {
	currentOrigin, _ := issueorigin.Parse(body)
	currentJob := jobEvidence(body)
	currentName := scheduledJob(body)
	series := stabilizationScope(body)
	runPrefix, _, _ := strings.Cut(currentOrigin.Source, "/actions/runs/")
	for _, issue := range c.issues {
		for _, previous := range issue.bodies {
			previousSeries := stabilizationScope(previous)
			if series != previousSeries {
				continue
			}
			if strings.Contains(previous, marker) {
				return issue, nil
			}
			if issue.item.Terminal || issue.item.Archived {
				continue
			}
			origin, ok := issueorigin.Parse(previous)
			if !ok {
				continue
			}
			if series != "" {
				if origin.Fingerprint != currentOrigin.Fingerprint {
					continue
				}
				if match == nil || issue.item.Number < match.item.Number {
					match = issue
				}
				continue
			}
			sameJob := currentName != "" && scheduledJob(previous) == currentName && origin.Kind == "doctor" && origin.Instance == "github-actions" && strings.HasPrefix(origin.Source, runPrefix+"/actions/runs/")
			if !issueorigin.SameDefect(previous, body) && !sameJob {
				continue
			}
			if origin.Source == currentOrigin.Source && currentJob != "" && jobEvidence(previous) == currentJob {
				return issue, nil
			}
			if match == nil || issue.item.Number < match.item.Number {
				match = issue
			}
		}
	}
	return nil, match
}

func (c *cloudDestination) updateFailure(ctx context.Context, issue *cloudIssue, key string, labels []string) error {
	const high = 1
	origin, _ := issueorigin.Parse(issue.item.Body)
	promote := issue.item.State == "Backlog" && origin.Kind != "worker"
	args := map[string]any{"project_id": c.project, "identifier": string(issue.item.WorkItemID), "request_id": "scheduled-priority-" + key, "expected_revision": strconv.FormatInt(int64(issue.item.Revision), 10)}
	if issue.item.Priority == nil || *issue.item.Priority > high {
		args["priority"] = high
	}
	if slices.Contains(labels, "ci-infrastructure-failure") && !slices.Contains(issue.item.Labels, "ci-infrastructure-failure") {
		args["labels"] = append(slices.Clone(issue.item.Labels), "ci-infrastructure-failure")
	}
	if len(args) > 4 || promote {
		if issue.item.Revision <= 0 {
			return errors.New("scheduled failure update has no observed native revision")
		}
	}
	var result struct {
		Revision tracker.Revision `json:"revision,string"`
	}
	if len(args) > 4 {
		if err := c.publish(ctx, "edit_item", args, &result); err != nil {
			return err
		}
		if priority, ok := args["priority"].(int); ok {
			issue.item.Priority = &priority
		}
		if labels, ok := args["labels"].([]string); ok {
			issue.item.Labels = labels
		}
		issue.item.Revision = result.Revision
	}
	if promote {
		args = map[string]any{"project_id": c.project, "identifier": string(issue.item.WorkItemID), "request_id": "scheduled-todo-" + key, "expected_revision": strconv.FormatInt(int64(issue.item.Revision), 10), "target_state": "Todo"}
		var moved struct {
			Data tracker.NativeIssue `json:"data"`
		}
		if err := c.publish(ctx, "move_item", args, &moved); err != nil {
			return err
		}
		issue.item.State = "Todo"
		issue.item.Revision = moved.Data.Revision
	}
	return nil
}

func scheduledJob(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if name, rest, ok := strings.Cut(strings.TrimPrefix(line, "Scheduled validation job **"), "** ("); ok && strings.HasPrefix(line, "Scheduled validation job **") && strings.Contains(rest, ") failed on development commit ") {
			return name
		}
	}
	return ""
}

func jobEvidence(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "Job: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Job: "))
		}
	}
	return ""
}

func (c *cloudDestination) publish(ctx context.Context, name string, args map[string]any, result any) error {
	if c.evidence != nil {
		if err := json.NewEncoder(c.evidence).Encode(struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		}{name, args}); err != nil {
			return errors.New("scheduled diagnostic evidence could not be retained")
		}
	}
	if err := boundCloudEvidence(args); err != nil {
		return err
	}
	return c.command(ctx, name, args, result)
}

func boundCloudEvidence(args map[string]any) error {
	const bodyLimit = 32 * 1024
	field := "description"
	body, ok := args[field].(string)
	if !ok {
		field = "body"
		body, ok = args[field].(string)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return errors.New("scheduled Cloud arguments could not be encoded")
	}
	if len(encoded) <= operatortool.MaxArgumentBytes && len(body) <= bodyLimit {
		return nil
	}
	prefix, evidence, found := strings.Cut(body, "\n\n```text\n")
	if !ok || !found {
		return errors.New("scheduled Cloud metadata exceeds the reporting argument byte limit")
	}
	end := strings.LastIndex(evidence, "\n```\n\nDiagnose this problem")
	if end < 0 {
		end = strings.LastIndex(evidence, "\n```\n\nThe release workflow")
	}
	if end < 0 {
		return errors.New("scheduled Cloud evidence could not be summarized")
	}
	suffix := evidence[end:]
	evidence = evidence[:end]
	const notice = "\n[remaining evidence omitted; complete evidence is retained in the linked logs and scheduled reporting artifact]"
	setExcerpt := func(size int) (bool, error) {
		for size > 0 && size < len(evidence) && !utf8.RuneStart(evidence[size]) {
			size--
		}
		excerpt := prefix + "\n\n```text\n" + evidence[:size] + notice + suffix
		args[field] = excerpt
		encoded, err := json.Marshal(args)
		if err != nil {
			return false, errors.New("scheduled Cloud arguments could not be encoded")
		}
		return len(encoded) <= operatortool.MaxArgumentBytes && len(excerpt) <= bodyLimit, nil
	}
	fits, err := setExcerpt(0)
	if err != nil {
		return err
	}
	if !fits {
		return errors.New("scheduled Cloud metadata exceeds the reporting argument byte limit")
	}
	low, high := 0, len(evidence)
	for low < high {
		mid := low + (high-low+1)/2
		fits, err := setExcerpt(mid)
		if err != nil {
			return err
		}
		if fits {
			low = mid
		} else {
			high = mid - 1
		}
	}
	_, err = setExcerpt(low)
	return err
}

func (c *cloudDestination) comment(ctx context.Context, issue *cloudIssue, body, key string) error {
	args := map[string]any{"project_id": c.project, "identifier": string(issue.item.WorkItemID), "request_id": "scheduled-comment-" + key, "body": body}
	var result json.RawMessage
	if err := c.publish(ctx, "add_comment", args, &result); err != nil {
		return err
	}
	retainedBody, ok := args["body"].(string)
	if !ok {
		return errors.New("scheduled Cloud comment body is not a string")
	}
	issue.bodies = append(issue.bodies, retainedBody)
	return nil
}

func scheduledConclusion(jobs []job) string {
	count := 0
	preflight, skipped, validated := false, false, false
	for _, j := range jobs {
		switch j.Name {
		case "Finalize scheduled validation":
			if j.Conclusion == "failure" {
				return "failure"
			}
			continue
		case "preflight":
			if j.Conclusion != "success" {
				return "failure"
			}
			preflight = true
		default:
			switch j.Conclusion {
			case "success":
				validated = true
			case "skipped":
				skipped = true
			default:
				return "failure"
			}
		}
		count++
	}
	if skipped {
		if preflight && !validated {
			return "skipped"
		}
		return "failure"
	}
	if count > 0 {
		return "success"
	}
	return "failure"
}

func (c *cloudDestination) recordValidation(ctx context.Context, getenv func(string) string, conclusion string) error {
	report := gate.ScheduledValidationReport{Repository: getenv("GITHUB_REPOSITORY"), RunID: getenv("GITHUB_RUN_ID"), RunAttempt: getenv("GITHUB_RUN_ATTEMPT"), HeadSHA: getenv("CI_DEVELOP_SHA"), Conclusion: conclusion}
	report.RunURL = "https://github.com/" + report.Repository + "/actions/runs/" + report.RunID + "/attempts/" + report.RunAttempt
	if !report.Valid() {
		return errors.New("scheduled validation report has no valid run identity")
	}
	var result json.RawMessage
	return c.publish(ctx, "record_scheduled_validation", map[string]any{"project_id": c.project, "request_id": "scheduled-validation-" + occurrenceKey(getenv, job{Name: "scheduled-validation"}, report.Repository), "scheduled_validation": report}, &result)
}

func (c *cloudDestination) success(ctx context.Context, getenv func(string) string) error {
	sha := getenv("CI_DEVELOP_SHA")
	if len(sha) != 40 {
		return errors.New("scheduled success has no pinned development commit")
	}
	runURL := fmt.Sprintf("%s/%s/actions/runs/%s/attempts/%s", getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID"), getenv("GITHUB_RUN_ATTEMPT"))
	var failures []error
	for _, issue := range c.issues {
		if issue.item.Terminal || issue.item.Archived {
			continue
		}
		scheduled := false
		for _, body := range issue.bodies {
			if stabilizationScope(body) != c.releaseSeries(getenv) {
				continue
			}
			origin, ok := issueorigin.Parse(body)
			if ok && origin.Kind == "doctor" && origin.Instance == "github-actions" && strings.HasPrefix(origin.Source, getenv("GITHUB_SERVER_URL")+"/"+getenv("GITHUB_REPOSITORY")+"/actions/runs/") {
				scheduled = true
			}
		}
		if !scheduled {
			continue
		}
		key := occurrenceKey(getenv, job{Name: "scheduled-success:" + string(issue.item.WorkItemID)}, sha)
		marker := occurrenceMarker(key)
		replayed := false
		for _, body := range issue.bodies {
			replayed = replayed || strings.Contains(body, marker)
		}
		if replayed {
			continue
		}
		body := fmt.Sprintf("Scheduled full validation passed for development commit `%s`.\n\nRun: %s\n\nThis is scheduled validation evidence only. It does not establish that this issue was repaired, landed, reviewed, or completed. Existing admission and completion owners retain authority.\n\n%s", sha, runURL, marker)
		if series := c.releaseSeries(getenv); series != "" {
			body += "\n\n" + stabilizationMarker(series) + "\nThe full suite reconciles this release series' historical failure set as green at this pinned SHA. Earlier occurrences remain historical evidence; this does not close repairs or remove human holds."
		}
		if err := c.comment(ctx, issue, body, key); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
