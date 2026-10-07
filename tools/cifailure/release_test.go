package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseFailureReportsNativeHighAndReusesOccurrence(t *testing.T) {
	for _, test := range []struct {
		name, environment, phase, diagnostic string
		held                                 bool
	}{
		{name: "staging smoke", environment: "staging", phase: "smoke", diagnostic: "FAIL deployed release identity"},
		{name: "production smoke", environment: "production", phase: "smoke", diagnostic: "FAIL deployed release identity"},
		{name: "staging schema refusal", environment: "staging", phase: "deploy", diagnostic: "hub database schema is newer than this Detent version: database=20261006212000 supported=20261006202000"},
		{name: "held staging schema refusal", environment: "staging", phase: "deploy", diagnostic: "hub database schema is newer than this Detent version: database=20261006212000 supported=20261006202000", held: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &fakeCloud{comments: map[string][]tracker.NativeComment{}}
			if test.held {
				body := issueorigin.Stamp("Previous staging deploy failure", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/37569254724/attempts/1", Fingerprint: "77159f1cf219325cdbb45d546373c6351b4306215ac3dace124b835627455e76"})
				item := cloudItem("wi_617", 617, body)
				item.State = "Blocked"
				priority := 0
				item.Priority = &priority
				item.Labels = []string{"release-deployment-failure", "ci-infrastructure-failure"}
				f.items = []tracker.NativeIssue{item}
			}
			var evidence bytes.Buffer
			destination := &cloudDestination{command: f.command, project: scheduledCloudProject, evidence: &evidence}
			attempt := 1
			getenv := func(key string) string {
				switch key {
				case "DETENT_RELEASE_ENVIRONMENT":
					return test.environment
				case "DETENT_RELEASE_FAILURE_PHASE":
					return test.phase
				case "DETENT_RELEASE_TAG":
					return "v1.2.4"
				case "GITHUB_RUN_ATTEMPT":
					return strconv.Itoa(attempt)
				default:
					return scheduledEnv(key)
				}
			}
			for range 2 {
				if err := reportReleaseFailure(t.Context(), strings.NewReader(test.diagnostic), getenv, destination); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.items) != 1 || len(f.writes) != 1 {
				t.Fatalf("duplicate occurrence: items=%d writes=%d", len(f.items), len(f.writes))
			}
			first := f.writes[0]
			field := "description"
			if test.held {
				field = "body"
				if first["identifier"] != "wi_617" || f.items[0].State != "Blocked" || *f.items[0].Priority != 0 {
					t.Fatalf("held occurrence changed its owner, lane or priority: %+v %+v", first, f.items[0])
				}
			} else if first["priority"] != 2 || first["state"] != "Todo" {
				t.Fatalf("create=%+v", first)
			}
			body, _ := first[field].(string)
			if !strings.Contains(body, "v1.2.4") || !strings.Contains(body, test.diagnostic) {
				t.Fatalf("release failure lost its version or diagnostic: %s", body)
			}
			attempt++
			if err := reportReleaseFailure(t.Context(), strings.NewReader(test.diagnostic), getenv, destination); err != nil {
				t.Fatal(err)
			}
			if len(f.items) != 1 || len(f.writes) != 2 || f.writes[1]["body"] == nil {
				t.Fatal("later occurrence did not reuse native work")
			}
		})
	}
}
