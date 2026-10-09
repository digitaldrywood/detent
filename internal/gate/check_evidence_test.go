package gate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCheckEvidenceComparison(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	environment := CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.6"}
	localCheck := CheckObservation{Scope: "lint", Command: "make lint", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), Environment: environment, StartedAt: at.Add(-time.Minute), FinishedAt: at.Add(-time.Second), DurationNS: 59e9, DurationResolutionNS: 1e9}
	failed := localCheck
	failed.HeadSHA = strings.Repeat("c", 40)
	failed.StartedAt, failed.FinishedAt, failed.ExitCode = at.Add(time.Minute), at.Add(2*time.Minute), 1
	scheduled := ScheduledEvidence{Schema: 1, Repository: "example/repo", OccurrenceKey: strings.Repeat("d", 64), RunID: "123", RunAttempt: "2", JobID: "456", JobName: "Lint", RunURL: "https://github.com/example/repo/actions/runs/123/attempts/2", JobURL: "https://github.com/example/repo/actions/jobs/456", HeadSHA: failed.HeadSHA, Conclusion: "failure", Checks: []CheckObservation{failed}}
	for _, test := range []struct {
		name, want string
		edit       func(*CommandResult, *ScheduledEvidence, *CheckObservation)
		missing    bool
	}{
		{name: "missing local receipt", want: "missing_local_evidence", missing: true},
		{name: "unobserved local exit remains unknown", want: "unknown", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) { l.ExitCode = -1 }},
		{name: "local command failed", want: "local_nonzero", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) { l.ExitCode = 7 }},
		{name: "changed tested tree", want: "different_content", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) { l.TreeSHA = strings.Repeat("e", 40) }},
		{name: "historical metadata unknown", want: "unknown", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) { l.Evidence = nil }},
		{name: "literal true has no checks", want: "unknown", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) {
			l.Command = "true"
			l.Evidence.Checks = nil
		}},
		{name: "full suite differs from short suite", want: "different_check_scope", edit: func(_ *CommandResult, _ *ScheduledEvidence, f *CheckObservation) {
			f.Scope = "unit-full"
			f.Command = "make test"
		}},
		{name: "same scope with different command", want: "different_check_scope", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) {
			l.Evidence.Checks[0].Command = "make another-lint"
		}},
		{name: "platform difference", want: "different_environment", edit: func(_ *CommandResult, _ *ScheduledEvidence, f *CheckObservation) { f.Environment.OS = "darwin" }},
		{name: "toolchain difference", want: "different_environment", edit: func(_ *CommandResult, _ *ScheduledEvidence, f *CheckObservation) {
			f.Environment.GoVersion = "go1.26.5"
		}},
		{name: "unknown environment", want: "unknown", edit: func(_ *CommandResult, _ *ScheduledEvidence, f *CheckObservation) { f.Environment.GoVersion = "" }},
		{name: "same tree matching check passed before failure", want: "local_pass_scheduled_failure"},
		{name: "checked record has different content", want: "unknown", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) {
			l.Evidence.Checks[0].TreeSHA = strings.Repeat("f", 40)
		}},
		{name: "failed local subcheck", want: "local_nonzero", edit: func(l *CommandResult, _ *ScheduledEvidence, _ *CheckObservation) { l.Evidence.Checks[0].ExitCode = 1 }},
		{name: "later local observation", want: "unknown", edit: func(_ *CommandResult, _ *ScheduledEvidence, f *CheckObservation) {
			f.StartedAt = at.Add(-2 * time.Minute)
		}},
		{name: "job skipped", want: "unknown", edit: func(_ *CommandResult, s *ScheduledEvidence, _ *CheckObservation) { s.Conclusion = "skipped" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			l := &CommandResult{Command: "make check-land", HeadSHA: localCheck.HeadSHA, TreeSHA: localCheck.TreeSHA, Evidence: &CommandEvidence{Environment: environment, Checks: []CheckObservation{localCheck}}}
			s, f := scheduled, failed
			if test.edit != nil {
				test.edit(l, &s, &f)
			}
			if test.missing {
				l = nil
			}
			c := CompareCheck(l, at, s, &f)
			if c.Outcome != test.want {
				t.Fatalf("comparison=%+v want=%s", c, test.want)
			}
		})
	}
	encoded, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, raw string
		want      int
	}{
		{"timestamped job log", "2026-10-06T12:00:00Z " + CheckEvidencePrefix + string(encoded), 1},
		{"credential in environment", strings.Replace(string(encoded), "go1.26.6", "/private/token", 1), 0},
		{"dirty tree", strings.Replace(string(encoded), failed.TreeSHA, "", 1), 0},
		{"private command", strings.Replace(string(encoded), "make lint", "make lint TOKEN=secret", 1), 0},
		{"malformed record", CheckEvidencePrefix + "{}", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := test.raw
			if !strings.Contains(raw, CheckEvidencePrefix) {
				raw = CheckEvidencePrefix + raw
			}
			if got := CheckObservations(raw); len(got) != test.want {
				t.Fatalf("checks=%+v", got)
			}
		})
	}
	if got := ParseScheduledEvidence(scheduled.Stamp()); len(got) != 1 || got[0].RunAttempt != "2" || got[0].JobID != "456" {
		t.Fatalf("scheduled=%+v", got)
	}
}

func TestPipelineCommandReceipts(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, command, public string
		exit                  int
		missingTimes          bool
	}{
		{"unstarted timing", "make check-land", "", 0, true},
		{"passed", "make check-land", "make check-land", 0, false},
		{"failed", "make check-land", "make check-land", 1, false},
		{"private command", "make check TOKEN=private", "[redacted]", 1, false},
		{"private path", "go test /private/customer", "[redacted]", 0, false},
		{"content command", "echo customer payload", "[redacted]", 0, false},
		{"content argument", "make check NOTE=customer", "[redacted]", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := CommandResult{Command: tt.command, HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), ExitCode: tt.exit, StartedAt: at, FinishedAt: at.Add(10 * time.Second), DurationNS: 10e9}
			if tt.missingTimes {
				result.StartedAt, result.FinishedAt, result.TimingStage = time.Time{}, time.Time{}, "finalization"
				if result.ValidPipeline("finalization") {
					t.Fatal("unstarted timing stage accepted")
				}
				return
			}
			if tt.exit != 0 {
				result.Stage = StageSourceFinalization
			}
			timing := result.PipelineTiming("finalization")
			if timing.ReceiptID == "" || timing.Stage != "finalization" || timing.Command != tt.public || timing.Execution != "executed" || timing.ExitCode == nil || *timing.ExitCode != tt.exit || timing.StartedAt != at || timing.FinishedAt != at.Add(10*time.Second) || timing.HeadSHA != result.HeadSHA || timing.TreeSHA != result.TreeSHA {
				t.Fatalf("receipt=%+v", timing)
			}
			if !timing.Valid() || !result.ValidPipeline("finalization") {
				t.Fatalf("invalid timing: %+v", timing)
			}
			reusedResult := result.Reused("landing_validation", at.Add(time.Minute))
			if reusedResult.Stage != result.Stage {
				t.Fatalf("reuse changed failure stage: %+v", reusedResult)
			}
			reused := reusedResult.PipelineTiming("landing_validation")
			if reused.Execution != "reused" || reused.ReusedReceiptID != timing.ReceiptID || reused.ReceiptID == timing.ReceiptID || reused.StartedAt != at.Add(time.Minute) || reused.FinishedAt != reused.StartedAt {
				t.Fatalf("reuse=%+v source=%+v", reused, timing)
			}
		})
	}
}
