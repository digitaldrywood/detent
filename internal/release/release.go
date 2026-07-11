package release

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrTagExists = errors.New("release tag already exists")

type Config struct {
	Enabled         bool
	MinMergedIssues int
	MaxAge          time.Duration
	RequireGreenCI  bool
	VersionBump     string
	RerunFlakyOnce  bool
	FlakyCheckNames []string
}

type Commit struct {
	SHA       string
	Message   string
	MergedAt  time.Time
	IssueRefs []string
}

type Check struct {
	Name       string
	Status     string
	Conclusion string
	RunID      int64
}

type Repository struct {
	Name      string
	HeadSHA   string
	LatestTag string
	LatestSHA string
	TaggedAt  time.Time
	Commits   []Commit
	Checks    []Check
}

type WorkflowRun struct {
	ID         int64
	URL        string
	Status     string
	Conclusion string
}

type Tag struct {
	Name    string
	SHA     string
	Message string
}

type Failure struct {
	Fingerprint string
	Title       string
	Body        string
}

type Backend interface {
	Inspect(context.Context) (Repository, error)
	CreateTag(context.Context, Tag) error
	ReleaseWorkflow(context.Context, string) (WorkflowRun, bool, error)
	RerunFailedChecks(context.Context, []Check) error
	EnsureFailureIssue(context.Context, Failure) (bool, error)
}

type Status struct {
	Enabled          bool
	State            string
	LastRelease      string
	LastReleaseAt    time.Time
	UnreleasedMerges int
	NextTriggerAt    time.Time
	CandidateSHA     string
	PendingTag       string
	LastError        string
}

type Decision struct {
	Action   string
	Reason   string
	Selected bool
}

type Coordinator interface {
	Evaluate(context.Context, time.Time) (Status, Decision)
}

type Service struct {
	cfg      Config
	backend  Backend
	mu       sync.Mutex
	status   Status
	rerunSHA string
	lastKey  string
	reported map[string]struct{}
}

func New(cfg Config, backend Backend) *Service {
	return &Service{
		cfg:     normalizeConfig(cfg),
		backend: backend,
		status: Status{
			Enabled: cfg.Enabled,
			State:   stateForEnabled(cfg.Enabled),
		},
		reported: make(map[string]struct{}),
	}
}

func (s *Service) Evaluate(ctx context.Context, now time.Time) (Status, Decision) {
	if s == nil {
		return Status{}, Decision{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Enabled || s.backend == nil {
		s.status = Status{Enabled: false, State: "disabled"}
		return s.status, Decision{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	repo, err := s.backend.Inspect(ctx)
	if err != nil {
		return s.failedStatus("inspect repository: " + err.Error()), s.decision("inspect_failed", err.Error(), false)
	}

	status := Status{
		Enabled:          true,
		State:            "waiting",
		LastRelease:      repo.LatestTag,
		LastReleaseAt:    repo.TaggedAt,
		UnreleasedMerges: mergedIssueCount(repo.Commits),
		CandidateSHA:     repo.HeadSHA,
	}
	if oldest := oldestMergedIssueAt(repo.Commits); !oldest.IsZero() {
		status.NextTriggerAt = oldest.Add(s.cfg.MaxAge)
	}

	if repo.LatestTag != "" && repo.LatestSHA == repo.HeadSHA {
		return s.observeRelease(ctx, repo, status)
	}
	if !triggered(s.cfg, status, now) {
		s.status = status
		return status, Decision{}
	}

	failed, pending := classifyChecks(repo.Checks)
	if len(pending) > 0 {
		status.State = "waiting_for_ci"
		s.status = status
		return status, s.decision("ci_pending", "candidate checks are still running", false)
	}
	if len(failed) > 0 {
		if s.shouldRerun(repo.HeadSHA, failed) {
			if err := s.backend.RerunFailedChecks(ctx, failed); err != nil {
				return s.fileFailure(ctx, repo, status, "ci_rerun_failed", err.Error())
			}
			s.rerunSHA = repo.HeadSHA
			status.State = "rerunning_ci"
			s.status = status
			return status, s.decision("ci_rerun", "reran configured flaky checks once", true)
		}
		return s.fileFailure(ctx, repo, status, "ci_failed", checkEvidence(failed))
	}
	if len(repo.Checks) == 0 {
		status.State = "waiting_for_ci"
		s.status = status
		return status, s.decision("ci_missing", "candidate has no reported checks", false)
	}

	next, err := NextVersion(repo.LatestTag, repo.Commits)
	if err != nil {
		return s.failedStatus(err.Error()), s.decision("version_failed", err.Error(), false)
	}
	tag := Tag{Name: next, SHA: repo.HeadSHA, Message: Changelog(next, repo.Commits)}
	if err := s.backend.CreateTag(ctx, tag); err != nil && !errors.Is(err, ErrTagExists) {
		return s.fileFailure(ctx, repo, status, "tag_failed", err.Error())
	}
	status.State = "release_pending"
	status.PendingTag = tag.Name
	s.status = status
	return status, s.decision("tag_created", "created "+tag.Name+" at "+repo.HeadSHA, true)
}

func (s *Service) observeRelease(ctx context.Context, repo Repository, status Status) (Status, Decision) {
	run, found, err := s.backend.ReleaseWorkflow(ctx, repo.LatestTag)
	if err != nil {
		return s.failedStatus(err.Error()), s.decision("release_watch_failed", err.Error(), false)
	}
	status.PendingTag = repo.LatestTag
	if !found || strings.EqualFold(run.Status, "queued") || strings.EqualFold(run.Status, "in_progress") {
		status.State = "release_pending"
		s.status = status
		return status, s.decision("release_pending", "waiting for release workflow for "+repo.LatestTag, false)
	}
	if !successfulConclusion(run.Conclusion) {
		return s.fileFailure(ctx, repo, status, "release_failed", fmt.Sprintf("workflow %d concluded %s (%s)", run.ID, run.Conclusion, run.URL))
	}
	status.State = "released"
	status.PendingTag = ""
	status.UnreleasedMerges = 0
	status.LastRelease = repo.LatestTag
	status.LastReleaseAt = repo.TaggedAt
	s.status = status
	return status, s.decision("release_succeeded", repo.LatestTag+" release workflow completed", true)
}

func (s *Service) fileFailure(ctx context.Context, repo Repository, status Status, kind string, evidence string) (Status, Decision) {
	fingerprint := kind + ":" + repo.Name + ":" + repo.HeadSHA
	title := "fix(release): investigate " + strings.ReplaceAll(kind, "_", " ") + " for " + repo.Name
	body := fmt.Sprintf("```detent-agent\nschema: 1\neffort: high\n```\n\nAuto-release stopped without retrying.\n\n- Repository: `%s`\n- Candidate: `%s`\n- Failure: `%s`\n\nEvidence:\n\n```text\n%s\n```\n\n<!-- detent-auto-release:%s -->", repo.Name, repo.HeadSHA, kind, evidence, fingerprint)
	if _, ok := s.reported[fingerprint]; ok {
		status.State = "failed"
		status.LastError = kind + ": " + evidence
		s.status = status
		return status, s.decision(kind, "failure issue already exists: "+evidence, false)
	}
	created, err := s.backend.EnsureFailureIssue(ctx, Failure{Fingerprint: fingerprint, Title: title, Body: body})
	if err != nil {
		status.LastError = kind + ": " + evidence + "; file issue: " + err.Error()
		status.State = "failed"
		s.status = status
		return status, s.decision("failure_issue_failed", status.LastError, false)
	}
	s.reported[fingerprint] = struct{}{}
	status.State = "failed"
	status.LastError = kind + ": " + evidence
	s.status = status
	reason := "failure issue already exists"
	if created {
		reason = "filed failure issue"
	}
	return status, s.decision(kind, reason+": "+evidence, false)
}

func (s *Service) failedStatus(message string) Status {
	s.status.State = "failed"
	s.status.LastError = message
	return s.status
}

func (s *Service) decision(action string, reason string, selected bool) Decision {
	key := action + "\x00" + reason
	if key == s.lastKey {
		return Decision{}
	}
	s.lastKey = key
	return Decision{Action: action, Reason: reason, Selected: selected}
}

func (s *Service) shouldRerun(sha string, failed []Check) bool {
	if !s.cfg.RerunFlakyOnce || sha == s.rerunSHA || len(failed) == 0 {
		return false
	}
	allowed := make(map[string]struct{}, len(s.cfg.FlakyCheckNames))
	for _, name := range s.cfg.FlakyCheckNames {
		allowed[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	for _, check := range failed {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(check.Name))]; !ok || check.RunID == 0 {
			return false
		}
	}
	return true
}

func normalizeConfig(cfg Config) Config {
	if cfg.MinMergedIssues <= 0 {
		cfg.MinMergedIssues = 5
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 24 * time.Hour
	}
	return cfg
}

func triggered(cfg Config, status Status, now time.Time) bool {
	if status.UnreleasedMerges >= cfg.MinMergedIssues {
		return true
	}
	return !status.NextTriggerAt.IsZero() && !now.Before(status.NextTriggerAt)
}

func classifyChecks(checks []Check) (failed []Check, pending []Check) {
	for _, check := range checks {
		if !strings.EqualFold(check.Status, "completed") {
			pending = append(pending, check)
			continue
		}
		if !successfulConclusion(check.Conclusion) {
			failed = append(failed, check)
		}
	}
	return failed, pending
}

func successfulConclusion(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "success", "neutral", "skipped":
		return true
	default:
		return false
	}
}

func mergedIssueCount(commits []Commit) int {
	refs := make(map[string]struct{})
	for _, commit := range commits {
		for _, ref := range commit.IssueRefs {
			ref = strings.TrimSpace(ref)
			if ref != "" {
				refs[ref] = struct{}{}
			}
		}
	}
	return len(refs)
}

func oldestMergedIssueAt(commits []Commit) time.Time {
	var oldest time.Time
	for _, commit := range commits {
		if len(commit.IssueRefs) == 0 || commit.MergedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || commit.MergedAt.Before(oldest) {
			oldest = commit.MergedAt
		}
	}
	return oldest
}

func checkEvidence(checks []Check) string {
	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		parts = append(parts, check.Name+"="+check.Conclusion)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func NextVersion(latest string, commits []Commit) (string, error) {
	major, minor, patch, err := parseVersion(latest)
	if err != nil {
		return "", err
	}
	minorBump := false
	for _, commit := range commits {
		kind := conventionalKind(commit.Message)
		if kind == "feat" {
			minorBump = true
			break
		}
	}
	if minorBump {
		minor++
		patch = 0
	} else {
		patch++
	}
	return fmt.Sprintf("v%d.%d.%d", major, minor, patch), nil
}

func parseVersion(value string) (int, int, int, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if value == "" {
		return 0, 0, 0, nil
	}
	core, _, _ := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("parse latest release %q: expected semantic version", value)
	}
	values := make([]int, 3)
	for i, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return 0, 0, 0, fmt.Errorf("parse latest release %q: expected semantic version", value)
		}
		values[i] = parsed
	}
	return values[0], values[1], values[2], nil
}

func Changelog(version string, commits []Commit) string {
	groups := map[string][]string{"Features": {}, "Fixes": {}, "Other": {}}
	order := []string{"Features", "Fixes", "Other"}
	for _, commit := range commits {
		subject := strings.TrimSpace(strings.SplitN(commit.Message, "\n", 2)[0])
		group := "Other"
		switch conventionalKind(subject) {
		case "feat":
			group = "Features"
		case "fix":
			group = "Fixes"
		}
		refs := uniqueStrings(commit.IssueRefs)
		if len(refs) > 0 {
			subject += " (" + strings.Join(refs, ", ") + ")"
		}
		groups[group] = append(groups[group], subject)
	}
	var out strings.Builder
	out.WriteString(version + "\n\n")
	for _, group := range order {
		if len(groups[group]) == 0 {
			continue
		}
		out.WriteString("## " + group + "\n\n")
		for _, subject := range groups[group] {
			out.WriteString("- " + subject + "\n")
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String())
}

func LatestTag(tags []string) string {
	best := ""
	bestMajor, bestMinor, bestPatch := -1, -1, -1
	for _, tag := range tags {
		major, minor, patch, err := parseVersion(tag)
		if err != nil {
			continue
		}
		if major > bestMajor || major == bestMajor && minor > bestMinor || major == bestMajor && minor == bestMinor && patch > bestPatch {
			best = tag
			bestMajor, bestMinor, bestPatch = major, minor, patch
		}
	}
	return best
}

func conventionalKind(message string) string {
	subject := strings.ToLower(strings.TrimSpace(strings.SplitN(message, "\n", 2)[0]))
	colon := strings.IndexByte(subject, ':')
	if colon < 0 {
		return ""
	}
	kind := subject[:colon]
	if open := strings.IndexByte(kind, '('); open >= 0 {
		kind = kind[:open]
	}
	return strings.TrimSuffix(kind, "!")
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stateForEnabled(enabled bool) string {
	if enabled {
		return "waiting"
	}
	return "disabled"
}
