package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/progresscheck"
)

func TestReleaseStabilization(t *testing.T) {
	t.Parallel()
	const diagnostic = "--- FAIL: TestOne (0s)\nFAIL\towner/repo/pkg\t0s"
	historicalKeys := map[string]string{
		"1/1": "5bc4a83dd3a44323ccffc900c1d66513e9a64025d830bf015c37660a49a15393",
		"1/2": "189173fde1dd012645b3c2f0d3e35876c9cf9187022535c52d057c73804412c8",
		"2/1": "6999012c15b7f25fce70caa2a294a86b85f38543c2f2e913672574b6a1871078",
		"2/2": "d6ee4eed6884bbec652a3c7cf8a438fd3ea65031a500c3ad9662e01eeaf63fe8",
	}
	for _, test := range []struct {
		name, legacy, hold, lost string
		terminal                 bool
	}{
		{name: "multi-job occurrences and green reconciliation"},
		{name: "active repair remains owned", legacy: "In Progress"},
		{name: "review repair remains owned", legacy: "Human Review"},
		{name: "blocked repair remains held", legacy: "Blocked"},
		{name: "stabilization human hold remains", hold: "Blocked"},
		{name: "terminal episode and replay", terminal: true},
		{name: "lost creation response", lost: "file_issue"},
		{name: "lost occurrence response", lost: "add_comment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCloud{lost: test.lost, decisions: map[string]progresscheck.CIDecision{
				"Coverage": {Outcome: progresscheck.CISource, Target: "TestOne", Reason: "Test assertion failed."},
				"Browser":  {Outcome: progresscheck.CIInfrastructure, Target: "runner connection", Reason: "Runner connection lost."},
			}}
			if test.legacy != "" {
				body := issueorigin.Stamp(diagnostic+"\n<!-- detent:no-dispatch -->", issueorigin.Origin{Kind: "worker", Instance: "runner", Source: "implementation", Fingerprint: issueorigin.Fingerprint("go-test:owner/repo/pkg:TestOne")})
				body += "\n<!-- detent-scheduled-occurrence:5bc4a83dd3a44323ccffc900c1d66513e9a64025d830bf015c37660a49a15393 -->"
				item := cloudItem("wi_repair", 10, body)
				item.State, item.Priority = test.legacy, new(0)
				f.items = append(f.items, item)
			}
			before := slices.Clone(f.items)
			var artifact bytes.Buffer
			check := gate.CheckObservation{Scope: "test", Command: "go test -timeout=60s ./pkg", HeadSHA: scheduledEnv("CI_DEVELOP_SHA"), TreeSHA: strings.Repeat("a", 40), Environment: gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.6"}, ExitCode: 1, StartedAt: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 15, 12, 0, 1, 0, time.UTC), DurationNS: 1e9, DurationResolutionNS: 1e9}
			rawCheck, err := json.Marshal(check)
			if err != nil {
				t.Fatal(err)
			}
			gh := &fakeGH{logs: map[int64]string{1: diagnostic + "\n" + gate.CheckEvidencePrefix + string(rawCheck), 2: "runner connection lost"}}
			jobs := []job{{ID: 1, Name: "Coverage", Conclusion: "failure", URL: "coverage-job"}, {ID: 2, Name: "Browser", Conclusion: "cancelled", URL: "browser-job"}}
			reportErrors := 0
			reportRun := func(series, run string, green bool) {
				t.Helper()
				input := slices.Clone(jobs)
				if green {
					for i := range input {
						input[i].Conclusion = "success"
					}
				}
				raw, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				getenv := func(key string) string {
					switch key {
					case "CI_RELEASE_BRANCH":
						return series
					case "GITHUB_RUN_ID":
						return run
					}
					return scheduledEnv(key)
				}
				destination := &cloudDestination{command: f.command, project: scheduledCloudProject, evidence: &artifact}
				err = reportTo(t.Context(), bytes.NewReader(raw), gh.command, getenv, destination)
				if err != nil {
					reportErrors++
					if test.lost == "" {
						t.Fatal(err)
					}
				}
			}
			reportRun("release/v1.2", "1", false)
			reportRun("release/v1.2", "1", false)
			index := len(before)
			if len(f.items) != index+1 {
				t.Fatalf("failed jobs created %d stabilization owners", len(f.items)-index)
			}
			owner := &f.items[index]
			if owner.Priority == nil || *owner.Priority != 1 || !slices.Contains(owner.Labels, "ci-infrastructure-failure") {
				t.Fatalf("mixed failure priority/attribution lost: %+v", owner)
			}
			if test.hold != "" {
				owner.State, owner.Priority = test.hold, new(0)
				owner.Body += "\n<!-- detent:no-dispatch -->"
			}
			if test.terminal {
				owner.State, owner.Terminal = "Done", true
				writes := len(f.writes)
				reportRun("release/v1.2", "1", false)
				if len(f.items) != index+1 || len(f.writes) != writes {
					t.Fatal("terminal replay wrote a new episode or changed history")
				}
			}
			reportRun("release/v1.2", "2", false)
			wantItems := index + 1
			if test.terminal {
				wantItems++
			}
			if len(f.items) != wantItems || index > 0 && !reflect.DeepEqual(f.items[:index], before) {
				t.Fatal("subsequent occurrence changed repair ownership or dispatched duplicate stabilization")
			}
			owner = &f.items[len(f.items)-1]
			if test.hold != "" && (owner.State != test.hold || *owner.Priority != 0 || !strings.Contains(owner.Body, "detent:no-dispatch")) {
				t.Fatal("stabilization hold or Urgent priority changed")
			}
			var retained strings.Builder
			retained.WriteString(owner.Body)
			for _, comment := range f.comments[string(owner.WorkItemID)] {
				retained.WriteByte('\n')
				retained.WriteString(comment.Body)
			}
			bodies := retained.String()
			if test.legacy != "" && !strings.Contains(bodies, "Existing repair: wi_repair") {
				t.Fatal("existing root-cause repair was not referenced")
			}
			for _, value := range []string{"coverage-job", "browser-job", "go-test:owner/repo/pkg:TestOne", "runner connection lost", "release/v1.2", "Historical failure set", "cherry-pick -x"} {
				if !strings.Contains(bodies, value) {
					t.Fatalf("lost evidence %q", value)
				}
			}
			evidence := gate.ParseScheduledEvidence(bodies)
			wantOccurrences := 4
			if test.terminal {
				wantOccurrences = 2
			}
			if len(evidence) != wantOccurrences {
				t.Fatalf("job/run evidence = %d; want %d", len(evidence), wantOccurrences)
			}
			seen := map[string]bool{}
			for _, occurrence := range evidence {
				expected := historicalKeys[occurrence.RunID+"/"+occurrence.JobID]
				if expected == "" || occurrence.OccurrenceKey != expected || seen[expected] || occurrence.HeadSHA != check.HeadSHA || occurrence.RunAttempt != "1" {
					t.Fatalf("changed replay identity or provenance: %+v", occurrence)
				}
				seen[expected] = true
				if !strings.Contains(bodies, "<!-- detent-scheduled-occurrence:"+expected+" -->") {
					t.Fatalf("lost historical replay marker %s", expected)
				}
				if occurrence.JobID == "1" && !reflect.DeepEqual(occurrence.Checks, []gate.CheckObservation{check}) {
					t.Fatal("lost exact-source command observations")
				}
			}
			reportRun("release/v1.3", "3", false)
			if len(f.items) != wantItems+1 {
				t.Fatal("second release series reused the first owner")
			}
			other := f.items[len(f.items)-1]
			otherComments := len(f.comments[string(other.WorkItemID)])
			releaseComments := len(f.comments[string(owner.WorkItemID)])
			reportRun("", "4", true)
			if len(f.comments[string(owner.WorkItemID)]) != releaseComments {
				t.Fatal("develop green reconciled a release owner")
			}
			reportRun("release/v1.2", "5", true)
			writes := len(f.writes)
			reportRun("release/v1.2", "5", true)
			if len(f.writes) != writes {
				t.Fatal("green replay wrote another occurrence")
			}
			wantErrors := 0
			if test.lost != "" {
				wantErrors = 1
			}
			if reportErrors != wantErrors {
				t.Fatalf("report errors = %d; want %d", reportErrors, wantErrors)
			}
			if len(f.comments[string(other.WorkItemID)]) != otherComments {
				t.Fatal("green evidence reconciled another release series")
			}
			comments := f.comments[string(owner.WorkItemID)]
			if !strings.Contains(comments[len(comments)-1].Body, "historical failure set as green") || owner.Terminal {
				t.Fatal("green failure set was not reconciled without completing work")
			}
			if test.legacy != "" && !reflect.DeepEqual(f.items[:index], before) {
				t.Fatal("green evidence changed existing repair")
			}
		})
	}
}
