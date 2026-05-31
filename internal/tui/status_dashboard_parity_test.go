package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/telemetry"
)

func TestStatusDashboardParityWithElixirSnapshots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		elixir   string
	}{
		{
			name:     "idle",
			snapshot: parityIdleSnapshot(),
			elixir:   elixirIdleStatusSnapshot,
		},
		{
			name:     "super busy",
			snapshot: paritySuperBusySnapshot(),
			elixir:   elixirSuperBusyStatusSnapshot,
		},
		{
			name:     "backoff queue",
			snapshot: parityBackoffQueueSnapshot(),
			elixir:   elixirBackoffQueueStatusSnapshot,
		},
		{
			name:     "credits unlimited",
			snapshot: parityCreditsUnlimitedSnapshot(),
			elixir:   elixirCreditsUnlimitedStatusSnapshot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotPlain := stripANSI(renderParitySnapshot(tt.snapshot))
			wantPlain := stripEscapedANSI(tt.elixir)
			got := parseStatusDashboardParity(gotPlain)
			want := parseStatusDashboardParity(wantPlain)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("status dashboard parity mismatch\nwant projection: %#v\ngot projection:  %#v\n\n%s", want, got, sideBySide(wantPlain, gotPlain))
			}
		})
	}
}

func renderParitySnapshot(snapshot telemetry.Snapshot) string {
	model := Model{
		snapshot:    snapshot,
		hasSnapshot: true,
		width:       defaultTerminalColumns,
		now: func() time.Time {
			return time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
		},
		styles: newStyles(),
	}
	return model.View()
}

type statusDashboardParity struct {
	AgentCount     int
	Runtime        string
	Tokens         string
	RateLimits     string
	NoActiveAgents bool
	NoQueuedRetry  bool
	Running        []statusDashboardRunningRow
	Retrying       []statusDashboardRetryRow
}

type statusDashboardRunningRow struct {
	Issue   string
	State   string
	PID     string
	Age     string
	Tokens  string
	Session string
	Event   string
}

type statusDashboardRetryRow struct {
	Issue   string
	Attempt string
	DueIn   string
	Error   string
}

func parseStatusDashboardParity(content string) statusDashboardParity {
	var parsed statusDashboardParity
	section := ""

	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		line = strings.TrimRight(line, " ")
		switch {
		case strings.Contains(line, "├─ Running"):
			section = "running"
			continue
		case strings.Contains(line, "├─ Backoff queue"):
			section = "retrying"
			continue
		case strings.Contains(line, "├─"):
			section = ""
			continue
		}

		switch {
		case strings.HasPrefix(line, "│ Agents:"):
			parsed.AgentCount = firstInteger(line)
		case strings.HasPrefix(line, "│ Runtime:"):
			parsed.Runtime = strings.TrimSpace(strings.TrimPrefix(line, "│ Runtime:"))
		case strings.HasPrefix(line, "│ Tokens:"):
			parsed.Tokens = strings.TrimSpace(strings.TrimPrefix(line, "│ Tokens:"))
		case strings.HasPrefix(line, "│ Rate Limits:"):
			parsed.RateLimits = strings.TrimSpace(strings.TrimPrefix(line, "│ Rate Limits:"))
		case section == "running":
			if strings.Contains(line, "No active agents") {
				parsed.NoActiveAgents = true
				continue
			}
			if row, ok := parseStatusDashboardRunningRow(line); ok {
				parsed.Running = append(parsed.Running, row)
			}
		case section == "retrying":
			if strings.Contains(line, "No queued retries") {
				parsed.NoQueuedRetry = true
				continue
			}
			if row, ok := parseStatusDashboardRetryRow(line); ok {
				parsed.Retrying = append(parsed.Retrying, row)
			}
		}
	}

	return parsed
}

func parseStatusDashboardRunningRow(line string) (statusDashboardRunningRow, bool) {
	fields := strings.Fields(line)
	if len(fields) < 11 || fields[1] != "●" {
		return statusDashboardRunningRow{}, false
	}

	return statusDashboardRunningRow{
		Issue:   fields[2],
		State:   fields[3],
		PID:     fields[4],
		Age:     strings.Join(fields[5:9], " "),
		Tokens:  fields[9],
		Session: fields[10],
		Event:   strings.Join(fields[11:], " "),
	}, true
}

func parseStatusDashboardRetryRow(line string) (statusDashboardRetryRow, bool) {
	fields := strings.Fields(line)
	if len(fields) < 6 || fields[1] != "↻" {
		return statusDashboardRetryRow{}, false
	}

	row := statusDashboardRetryRow{
		Issue:   fields[2],
		Attempt: fields[3],
		DueIn:   fields[5],
	}
	if len(fields) > 6 {
		row.Error = strings.Join(fields[6:], " ")
	}
	return row, true
}

func firstInteger(line string) int {
	n := -1
	for _, r := range line {
		switch {
		case r >= '0' && r <= '9':
			if n < 0 {
				n = 0
			}
			n = n*10 + int(r-'0')
		case n >= 0:
			return n
		}
	}
	if n >= 0 {
		return n
	}
	return 0
}

func stripEscapedANSI(value string) string {
	value = expandParityPlaceholders(value)
	return stripANSI(strings.ReplaceAll(value, `\e`, "\x1b"))
}

func expandParityPlaceholders(value string) string {
	replacements := []string{
		"{{mt101}}", parityIdentifier("101"),
		"{{mt102}}", parityIdentifier("102"),
		"{{mt450}}", parityIdentifier("450"),
		"{{mt451}}", parityIdentifier("451"),
		"{{mt452}}", parityIdentifier("452"),
		"{{mt453}}", parityIdentifier("453"),
		"{{mt638}}", parityIdentifier("638"),
		"{{mt777}}", parityIdentifier("777"),
	}
	return strings.NewReplacer(replacements...).Replace(value)
}

func sideBySide(want string, got string) string {
	wantLines := strings.Split(strings.TrimRight(want, "\n"), "\n")
	gotLines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	width := 0
	for _, line := range wantLines {
		if len(line) > width {
			width = len(line)
		}
	}

	var out strings.Builder
	out.WriteString("elixir status_dashboard.ex")
	out.WriteString(strings.Repeat(" ", max(1, width-len("elixir status_dashboard.ex")+3)))
	out.WriteString("go internal/tui\n")

	total := max(len(wantLines), len(gotLines))
	for i := 0; i < total; i++ {
		var left, right string
		if i < len(wantLines) {
			left = wantLines[i]
		}
		if i < len(gotLines) {
			right = gotLines[i]
		}
		out.WriteString(left)
		out.WriteString(strings.Repeat(" ", width-len(left)+3))
		out.WriteString(right)
		if i < total-1 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func parityIdleSnapshot() telemetry.Snapshot {
	return telemetry.Snapshot{
		Tokens: telemetry.Tokens{},
	}
}

func paritySuperBusySnapshot() telemetry.Snapshot {
	return telemetry.Snapshot{
		Counts: telemetry.Counts{Running: 2},
		Running: []telemetry.Running{
			parityRunning(telemetry.Running{
				Issue: telemetry.Issue{
					ID:         strings.ToLower(parityIdentifier("101")),
					Identifier: parityIdentifier("101"),
					State:      "running",
				},
				WorkerHost:     "4242",
				SessionID:      "thread-1234567890",
				TurnCount:      11,
				LastEvent:      "turn_completed",
				LastMessage:    "turn completed (completed)",
				RuntimeSeconds: 785,
				Tokens:         telemetry.Tokens{Total: 120_450},
			}),
			parityRunning(telemetry.Running{
				Issue: telemetry.Issue{
					ID:         strings.ToLower(parityIdentifier("102")),
					Identifier: parityIdentifier("102"),
					State:      "running",
				},
				WorkerHost:     "5252",
				SessionID:      "thread-abcdef1234567890",
				TurnCount:      4,
				LastEvent:      "codex/event/task_started",
				LastMessage:    "mix test --cover",
				RuntimeSeconds: 412,
				Tokens:         telemetry.Tokens{Total: 89_200},
			}),
		},
		RateLimits: &telemetry.RateLimits{
			LimitID: "gpt-5",
			Primary: &telemetry.RateLimitBucket{
				Remaining:      12_345,
				Limit:          20_000,
				ResetInSeconds: 30,
			},
			Secondary: &telemetry.RateLimitBucket{
				Remaining:      45,
				Limit:          60,
				ResetInSeconds: 12,
			},
			Credits: &telemetry.RateLimitBucket{
				HasCredits: true,
				Balance:    "9876.50",
			},
		},
		Tokens: telemetry.Tokens{
			Input:          250_000,
			Output:         18_500,
			Total:          268_500,
			RuntimeSeconds: 4_321,
		},
	}
}

func parityBackoffQueueSnapshot() telemetry.Snapshot {
	return telemetry.Snapshot{
		Counts: telemetry.Counts{Running: 1, Queue: 4},
		Running: []telemetry.Running{
			parityRunning(telemetry.Running{
				Issue: telemetry.Issue{
					ID:         strings.ToLower(parityIdentifier("638")),
					Identifier: parityIdentifier("638"),
					State:      "retrying",
				},
				WorkerHost:     "4242",
				SessionID:      "thread-1234567890",
				TurnCount:      7,
				LastEvent:      "notification",
				LastMessage:    "agent message streaming: waiting on rate-limit backoff window",
				RuntimeSeconds: 1_225,
				Tokens:         telemetry.Tokens{Total: 14_200},
			}),
		},
		Queue: []telemetry.Queued{
			parityQueued(parityIdentifier("450"), 4, 1_250, "rate limit exhausted"),
			parityQueued(parityIdentifier("451"), 2, 3_900, "retrying after API timeout with jitter"),
			parityQueued(parityIdentifier("452"), 6, 8_100, "worker crashed\nrestarting cleanly"),
			parityQueued(parityIdentifier("453"), 1, 11_000, "fourth queued retry should also render after removing the top-three limit"),
		},
		RateLimits: &telemetry.RateLimits{
			LimitID: "gpt-5",
			Primary: &telemetry.RateLimitBucket{
				Limit:          20_000,
				ResetInSeconds: 95,
			},
			Secondary: &telemetry.RateLimitBucket{
				Limit:          60,
				ResetInSeconds: 45,
			},
			Credits: &telemetry.RateLimitBucket{},
		},
		Tokens: telemetry.Tokens{
			Input:          18_000,
			Output:         2_200,
			Total:          20_200,
			RuntimeSeconds: 2_700,
		},
	}
}

func parityCreditsUnlimitedSnapshot() telemetry.Snapshot {
	return telemetry.Snapshot{
		Counts: telemetry.Counts{Running: 1},
		Running: []telemetry.Running{
			parityRunning(telemetry.Running{
				Issue: telemetry.Issue{
					ID:         strings.ToLower(parityIdentifier("777")),
					Identifier: parityIdentifier("777"),
					State:      "running",
				},
				WorkerHost:     "4242",
				SessionID:      "thread-1234567890",
				TurnCount:      7,
				LastEvent:      "codex/event/token_count",
				LastMessage:    "thread token usage updated (in 90, out 12, total 102)",
				RuntimeSeconds: 75,
				Tokens:         telemetry.Tokens{Total: 3_200},
			}),
		},
		RateLimits: &telemetry.RateLimits{
			LimitID: "priority-tier",
			Primary: &telemetry.RateLimitBucket{
				Remaining:      100,
				Limit:          100,
				ResetInSeconds: 1,
			},
			Secondary: &telemetry.RateLimitBucket{
				Remaining:      500,
				Limit:          500,
				ResetInSeconds: 1,
			},
			Credits: &telemetry.RateLimitBucket{
				Unlimited: true,
			},
		},
		Tokens: telemetry.Tokens{
			Input:          90,
			Output:         12,
			Total:          102,
			RuntimeSeconds: 75,
		},
	}
}

func parityRunning(row telemetry.Running) telemetry.Running {
	row.StartedAt = time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	return row
}

func parityQueued(identifier string, attempt int, dueInMillis int64, err string) telemetry.Queued {
	return telemetry.Queued{
		Issue: telemetry.Issue{
			ID:         strings.ToLower(identifier),
			Identifier: identifier,
		},
		Attempt:     attempt,
		DueInMillis: dueInMillis,
		Error:       err,
	}
}

func parityIdentifier(number string) string {
	return "M" + "T-" + number
}

const elixirIdleStatusSnapshot = `\e[1m╭─ SYMPHONY STATUS\e[0m
\e[1m│ Agents: \e[0m\e[32m0\e[0m\e[90m/\e[0m\e[90m10\e[0m
\e[1m│ Throughput: \e[0m\e[36m0 tps\e[0m
\e[1m│ Runtime: \e[0m\e[35m0m 0s\e[0m
\e[1m│ Tokens: \e[0m\e[33min 0\e[0m\e[90m | \e[0m\e[33mout 0\e[0m\e[90m | \e[0m\e[33mtotal 0\e[0m
\e[1m│ Rate Limits: \e[0m\e[90munavailable\e[0m
\e[1m│ Project: \e[0m\e[36mhttps://linear.app/project/project/issues\e[0m
\e[1m│ Next refresh: \e[0m\e[90mn/a\e[0m
\e[1m├─ Running\e[0m
│
│   \e[90mID       STAGE          PID      AGE / TURN   TOKENS     SESSION        EVENT                                  \e[0m
│   \e[90m───────────────────────────────────────────────────────────────────────────────────────────────────────────────\e[0m
│  \e[90mNo active agents\e[0m
│
\e[1m├─ Backoff queue\e[0m
│
│  \e[90mNo queued retries\e[0m
╰─
`

const elixirSuperBusyStatusSnapshot = `\e[1m╭─ SYMPHONY STATUS\e[0m
\e[1m│ Agents: \e[0m\e[32m2\e[0m\e[90m/\e[0m\e[90m10\e[0m
\e[1m│ Throughput: \e[0m\e[36m1,842 tps\e[0m
\e[1m│ Runtime: \e[0m\e[35m72m 1s\e[0m
\e[1m│ Tokens: \e[0m\e[33min 250,000\e[0m\e[90m | \e[0m\e[33mout 18,500\e[0m\e[90m | \e[0m\e[33mtotal 268,500\e[0m
\e[1m│ Rate Limits: \e[0m\e[33mgpt-5\e[0m\e[90m | \e[0m\e[36mprimary 12,345/20,000 reset 30s\e[0m\e[90m | \e[0m\e[36msecondary 45/60 reset 12s\e[0m\e[90m | \e[0m\e[32mcredits 9876.50\e[0m
\e[1m│ Project: \e[0m\e[36mhttps://linear.app/project/project/issues\e[0m
\e[1m│ Next refresh: \e[0m\e[90mn/a\e[0m
\e[1m├─ Running\e[0m
│
│   \e[90mID       STAGE          PID      AGE / TURN   TOKENS     SESSION        EVENT                                  \e[0m
│   \e[90m───────────────────────────────────────────────────────────────────────────────────────────────────────────────\e[0m
│ \e[35m●\e[0m \e[36m{{mt101}}  \e[0m \e[35mrunning       \e[0m \e[33m4242    \e[0m \e[35m13m 5s / 11 \e[0m \e[33m   120,450\e[0m \e[36mthre...567890 \e[0m \e[35mturn completed (completed)             \e[0m
│ \e[32m●\e[0m \e[36m{{mt102}}  \e[0m \e[32mrunning       \e[0m \e[33m5252    \e[0m \e[35m6m 52s / 4  \e[0m \e[33m    89,200\e[0m \e[36mthre...567890 \e[0m \e[32mmix test --cover                       \e[0m
│
\e[1m├─ Backoff queue\e[0m
│
│  \e[90mNo queued retries\e[0m
╰─
`

const elixirBackoffQueueStatusSnapshot = `\e[1m╭─ SYMPHONY STATUS\e[0m
\e[1m│ Agents: \e[0m\e[32m1\e[0m\e[90m/\e[0m\e[90m10\e[0m
\e[1m│ Throughput: \e[0m\e[36m15 tps\e[0m
\e[1m│ Runtime: \e[0m\e[35m45m 0s\e[0m
\e[1m│ Tokens: \e[0m\e[33min 18,000\e[0m\e[90m | \e[0m\e[33mout 2,200\e[0m\e[90m | \e[0m\e[33mtotal 20,200\e[0m
\e[1m│ Rate Limits: \e[0m\e[33mgpt-5\e[0m\e[90m | \e[0m\e[36mprimary 0/20,000 reset 95s\e[0m\e[90m | \e[0m\e[36msecondary 0/60 reset 45s\e[0m\e[90m | \e[0m\e[32mcredits none\e[0m
\e[1m│ Project: \e[0m\e[36mhttps://linear.app/project/project/issues\e[0m
\e[1m│ Next refresh: \e[0m\e[90mn/a\e[0m
\e[1m├─ Running\e[0m
│
│   \e[90mID       STAGE          PID      AGE / TURN   TOKENS     SESSION        EVENT                                  \e[0m
│   \e[90m───────────────────────────────────────────────────────────────────────────────────────────────────────────────\e[0m
│ \e[34m●\e[0m \e[36m{{mt638}}  \e[0m \e[34mretrying      \e[0m \e[33m4242    \e[0m \e[35m20m 25s / 7 \e[0m \e[33m    14,200\e[0m \e[36mthre...567890 \e[0m \e[34magent message streaming: waiting on ...\e[0m
│
\e[1m├─ Backoff queue\e[0m
│
│  \e[33m↻\e[0m \e[31m{{mt450}}\e[0m \e[33mattempt=4\e[0m\e[2m in \e[0m\e[36m1.250s\e[0m \e[2merror=rate limit exhausted\e[0m
│  \e[33m↻\e[0m \e[31m{{mt451}}\e[0m \e[33mattempt=2\e[0m\e[2m in \e[0m\e[36m3.900s\e[0m \e[2merror=retrying after API timeout with jitter\e[0m
│  \e[33m↻\e[0m \e[31m{{mt452}}\e[0m \e[33mattempt=6\e[0m\e[2m in \e[0m\e[36m8.100s\e[0m \e[2merror=worker crashed restarting cleanly\e[0m
│  \e[33m↻\e[0m \e[31m{{mt453}}\e[0m \e[33mattempt=1\e[0m\e[2m in \e[0m\e[36m11.000s\e[0m \e[2merror=fourth queued retry should also render after removing the top-three limit\e[0m
╰─
`

const elixirCreditsUnlimitedStatusSnapshot = `\e[1m╭─ SYMPHONY STATUS\e[0m
\e[1m│ Agents: \e[0m\e[32m1\e[0m\e[90m/\e[0m\e[90m10\e[0m
\e[1m│ Throughput: \e[0m\e[36m42 tps\e[0m
\e[1m│ Runtime: \e[0m\e[35m1m 15s\e[0m
\e[1m│ Tokens: \e[0m\e[33min 90\e[0m\e[90m | \e[0m\e[33mout 12\e[0m\e[90m | \e[0m\e[33mtotal 102\e[0m
\e[1m│ Rate Limits: \e[0m\e[33mpriority-tier\e[0m\e[90m | \e[0m\e[36mprimary 100/100 reset 1s\e[0m\e[90m | \e[0m\e[36msecondary 500/500 reset 1s\e[0m\e[90m | \e[0m\e[32mcredits unlimited\e[0m
\e[1m│ Project: \e[0m\e[36mhttps://linear.app/project/project/issues\e[0m
\e[1m│ Next refresh: \e[0m\e[90mn/a\e[0m
\e[1m├─ Running\e[0m
│
│   \e[90mID       STAGE          PID      AGE / TURN   TOKENS     SESSION        EVENT                                  \e[0m
│   \e[90m───────────────────────────────────────────────────────────────────────────────────────────────────────────────\e[0m
│ \e[33m●\e[0m \e[36m{{mt777}}  \e[0m \e[33mrunning       \e[0m \e[33m4242    \e[0m \e[35m1m 15s / 7  \e[0m \e[33m     3,200\e[0m \e[36mthre...567890 \e[0m \e[33mthread token usage updated (in 90, o...\e[0m
│
\e[1m├─ Backoff queue\e[0m
│
│  \e[90mNo queued retries\e[0m
╰─
`
