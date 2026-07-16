//go:build !windows

package detent_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestResolveReleaseTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scenario   string
		wantTag    string
		wantCalls  int
		wantDelays string
		wantErr    bool
		wantStderr []string
	}{
		{
			name:       "retries transient failures",
			scenario:   "transient",
			wantTag:    "v1.2.3\n",
			wantCalls:  3,
			wantDelays: "2\n4\n",
			wantStderr: []string{
				"HTTP 503",
				"retrying in 2s",
			},
		},
		{
			name:       "stops after bounded attempts",
			scenario:   "exhausted",
			wantCalls:  4,
			wantDelays: "2\n4\n8\n",
			wantErr:    true,
			wantStderr: []string{
				"HTTP 500",
				"after 4 attempts",
			},
		},
		{
			name:      "fails permanent error immediately",
			scenario:  "permanent",
			wantCalls: 1,
			wantErr:   true,
			wantStderr: []string{
				"HTTP 404",
				"permanent release lookup failure",
			},
		},
		{
			name:      "fails empty tag immediately",
			scenario:  "empty",
			wantCalls: 1,
			wantErr:   true,
			wantStderr: []string{
				"release lookup returned an empty tag",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			countPath := filepath.Join(tmp, "count")
			delaysPath := filepath.Join(tmp, "delays")
			ghPath := filepath.Join(tmp, "gh")
			if err := os.WriteFile(ghPath, []byte(fakeGHScript), 0o755); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			sleepPath := filepath.Join(tmp, "sleep")
			if err := os.WriteFile(sleepPath, []byte(fakeSleepScript), 0o755); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cmd := exec.Command("bash", "./scripts/resolve-release-tag.sh")
			cmd.Env = append(os.Environ(),
				"DETENT_RELEASE_LOOKUP_GH="+ghPath,
				"DETENT_RELEASE_LOOKUP_SLEEP="+sleepPath,
				"GITHUB_REPOSITORY=digitaldrywood/detent",
				"FAKE_GH_COUNT="+countPath,
				"FAKE_SLEEP_DELAYS="+delaysPath,
				"FAKE_GH_SCENARIO="+tt.scenario,
			)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Run() error = %v, wantErr %v\nstdout:\n%s\nstderr:\n%s", err, tt.wantErr, stdout.String(), stderr.String())
			}
			if stdout.String() != tt.wantTag {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantTag)
			}

			countData, readErr := os.ReadFile(countPath)
			if readErr != nil {
				t.Fatalf("ReadFile(count) error = %v", readErr)
			}
			calls, convErr := strconv.Atoi(strings.TrimSpace(string(countData)))
			if convErr != nil {
				t.Fatalf("Atoi(%q) error = %v", countData, convErr)
			}
			if calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tt.wantCalls)
			}
			delays, readErr := os.ReadFile(delaysPath)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("ReadFile(delays) error = %v", readErr)
			}
			if string(delays) != tt.wantDelays {
				t.Errorf("delays = %q, want %q", delays, tt.wantDelays)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want substring %q", stderr.String(), want)
				}
			}
		})
	}
}

const fakeGHScript = `#!/usr/bin/env bash
set -euo pipefail

count=0
if [[ -f "$FAKE_GH_COUNT" ]]; then
	count="$(<"$FAKE_GH_COUNT")"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$FAKE_GH_COUNT"

case "$FAKE_GH_SCENARIO:$count" in
	transient:1|transient:2)
		echo "gh: HTTP 503: Service Unavailable" >&2
		exit 1
		;;
	transient:*)
		echo "v1.2.3"
		;;
	exhausted:*)
		echo "gh: HTTP 500: Internal Server Error" >&2
		exit 1
		;;
	permanent:*)
		echo "gh: HTTP 404: Not Found" >&2
		exit 1
		;;
	empty:*)
		printf '  \n'
		;;
esac
`

const fakeSleepScript = `#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$1" >> "$FAKE_SLEEP_DELAYS"
`
