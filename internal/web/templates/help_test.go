package templates

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestHelpTermsCoverDashboardSectionsAndMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		term helpTerm
		unit string
	}{
		{name: "running", term: helpRunning},
		{name: "queue", term: helpQueue},
		{name: "backoff queue", term: helpBackoffQueue},
		{name: "blocked", term: helpBlocked},
		{name: "completed", term: helpCompleted},
		{name: "budget", term: helpBudget, unit: "USD"},
		{name: "rate limits", term: helpRateLimits},
		{name: "tokens", term: helpTokens, unit: "tokens"},
		{name: "throughput", term: helpThroughput, unit: "tps"},
		{name: "runtime", term: helpRuntime},
		{name: "age turn", term: helpAgeTurn},
		{name: "session", term: helpSession},
		{name: "event", term: helpEvent},
		{name: "diff", term: helpDiff},
		{name: "projected spend", term: helpProjectedSpend, unit: "USD"},
		{name: "daily cap", term: helpDailyCap, unit: "USD"},
		{name: "issue cap", term: helpIssueCap, unit: "USD"},
		{name: "primary rate bucket", term: helpPrimaryRateBucket},
		{name: "secondary rate bucket", term: helpSecondaryRateBucket},
		{name: "credits rate bucket", term: helpCreditsRateBucket},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry, ok := helpDefinitions[tt.term]
			if !ok {
				t.Fatalf("helpDefinitions[%q] missing", tt.term)
			}
			if entry.Label == "" {
				t.Fatalf("helpDefinitions[%q].Label is empty", tt.term)
			}
			if entry.Description == "" {
				t.Fatalf("helpDefinitions[%q].Description is empty", tt.term)
			}
			if tt.unit != "" && !containsFold(entry.Description, tt.unit) {
				t.Fatalf("helpDefinitions[%q].Description = %q, want unit %q", tt.term, entry.Description, tt.unit)
			}
		})
	}
}

func TestHelpIDIncludesScopeAndTerm(t *testing.T) {
	t.Parallel()

	if got := helpID(helpThroughput, "metric-card"); got != "help-metric-card-throughput" {
		t.Fatalf("helpID() = %q, want %q", got, "help-metric-card-throughput")
	}

	if got := helpID(helpRateLimits, "Codex Rate Limits"); got != "help-codex-rate-limits-rate-limits" {
		t.Fatalf("helpID() = %q, want %q", got, "help-codex-rate-limits-rate-limits")
	}
}

func TestHelpScriptUsesStableSharedPopoverUtility(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := helpScript().Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		"data-help-trigger",
		"data-help-tooltip",
		"currentHelpTerm",
		"cancelHelpHide",
		"showDelay",
		"hideDelay",
		"pointerover",
		"pointerout",
		"positionHelpTooltip",
		"availableBelow",
		"maxHeight",
		"aria-describedby",
		`document.addEventListener("htmx:afterSettle"`,
		"Escape",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("help script missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `document.body.addEventListener("htmx:afterSettle"`) {
		t.Fatalf("help script installs htmx settle listener on document.body:\n%s", html)
	}
	if strings.Contains(html, "mousemove") {
		t.Fatalf("help script should not reposition on mousemove:\n%s", html)
	}
	if strings.Contains(html, "})()\n\n\t\t(()") {
		t.Fatalf("help script IIFEs must be semicolon separated:\n%s", html)
	}
}

func TestHelpIconUsesSharedTooltipMetadata(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := helpIcon(helpRunning, "dashboard").Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		`data-help-tip`,
		`data-help-trigger`,
		`data-help-term="running"`,
		`data-help-title="Running"`,
		`data-help-description="Issues currently assigned to Codex`,
		`aria-label="Help: Running"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("help icon missing %q:\n%s", want, html)
		}
	}
	for _, unwanted := range []string{
		`data-popover`,
		`data-popover-panel`,
		`class="popover-panel help-tip-panel"`,
		`id="help-dashboard-running"`,
		`role="tooltip"`,
	} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("help icon still renders per-trigger panel %q:\n%s", unwanted, html)
		}
	}
}

func TestHelpTooltipHostRendersSharedBodyTooltip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := helpTooltipHost().Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		`id="help-tooltip"`,
		`role="tooltip"`,
		`aria-hidden="true"`,
		`data-help-tooltip`,
		`data-help-tooltip-title`,
		`data-help-tooltip-description`,
		`class="popover-panel help-tooltip"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("help tooltip host missing %q:\n%s", want, html)
		}
	}
}

func TestHelpCSSKeepsPanelsPointerTransparent(t *testing.T) {
	t.Parallel()

	css, err := os.ReadFile("../../../static/css/input.css")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(css)

	for _, want := range []string{
		".popover-panel",
		"pointer-events: none;",
		`.help-tooltip[data-open="true"]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("CSS missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, ".help-tip:hover .help-tip-panel") {
		t.Fatalf("CSS hover-open rule can fight JS placement:\n%s", text)
	}
}

func containsFold(value string, substr string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(substr))
}
