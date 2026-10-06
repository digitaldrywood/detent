package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseFailureReportsNativeHighAndReusesOccurrence(t *testing.T) {
	for _, environment := range []string{"staging", "production"} {
		t.Run(environment, func(t *testing.T) {
			f := &fakeCloud{comments: map[string][]tracker.NativeComment{}}
			var evidence bytes.Buffer
			destination := &cloudDestination{command: f.command, project: scheduledCloudProject, evidence: &evidence}
			attempt := 1
			getenv := func(key string) string {
				switch key {
				case "DETENT_RELEASE_ENVIRONMENT":
					return environment
				case "DETENT_RELEASE_FAILURE_PHASE":
					return "smoke"
				case "DETENT_RELEASE_TAG":
					return "v1.2.4"
				case "GITHUB_RUN_ATTEMPT":
					return strconv.Itoa(attempt)
				default:
					return scheduledEnv(key)
				}
			}
			for range 2 {
				if err := reportReleaseFailure(t.Context(), strings.NewReader("FAIL deployed release identity"), getenv, destination); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.items) != 1 || len(f.writes) != 1 {
				t.Fatalf("duplicate occurrence: items=%d writes=%d", len(f.items), len(f.writes))
			}
			created := f.writes[0]
			if created["priority"] != 2 || created["state"] != "Todo" || !strings.Contains(created["description"].(string), "v1.2.4") {
				t.Fatalf("create=%+v", created)
			}
			attempt++
			if err := reportReleaseFailure(t.Context(), strings.NewReader("FAIL deployed release identity"), getenv, destination); err != nil {
				t.Fatal(err)
			}
			if len(f.items) != 1 || len(f.writes) != 2 || f.writes[1]["body"] == nil {
				t.Fatal("later occurrence did not reuse native work")
			}
		})
	}
}
