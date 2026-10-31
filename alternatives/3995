package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/issueorigin"
)

var stabilizationSeries = regexp.MustCompile(`^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func (c *cloudDestination) releaseSeries(getenv func(string) string) string {
	series := getenv("CI_RELEASE_BRANCH")
	if c.project == scheduledCloudProject && getenv("GITHUB_REPOSITORY") == "digitaldrywood/detent" && stabilizationSeries.MatchString(series) {
		return series
	}
	return ""
}

func stabilizationMarker(series string) string {
	return "Release stabilization: `" + series + "`"
}

func stabilizationScope(body string) string {
	return issueorigin.ReleaseStabilizationSeries(body)
}

func (c *cloudDestination) stabilizationContext(series string, jobs []job, body string) string {
	var context strings.Builder
	fmt.Fprintf(&context, "\n\n%s\n\nHistorical failure set for this run (not current reproduction):\n", stabilizationMarker(series))
	for _, j := range jobs {
		if j.Conclusion != "success" && (j.Name != "Finalize scheduled validation" || j.Conclusion == "failure") {
			fmt.Fprintf(&context, "- %s (%s): %s\n", j.Name, j.Conclusion, j.URL)
		}
	}
	context.WriteString("\nThis release series has one open stabilization owner. Subsequent job occurrences update this owner. Group repairs by reproduced root cause, not job count; keep historical diagnostics separate from failures reproduced on the implementation-start base. Reference and reuse existing repairs without restarting them, changing ownership, review lanes or human holds. Validate one integrated candidate using applicable exact-source receipts. Land fixes on develop first, then cherry-pick -x them onto the frozen release branch through scripts/release-pick.sh as signed direct pushes; never open pull requests. Every configured full-suite check must pass before tagging; signed artifacts, staging and production smokes and main promotion retain their existing owners. Develop landing remains independent.\n")
	legacy := issueorigin.Stamp(body, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Fingerprint: legacyJobFingerprint("scheduled-ci:digitaldrywood/detent:" + scheduledJob(body))})
	for _, issue := range c.issues {
		if issue.item.Terminal || issue.item.Archived {
			continue
		}
		for _, previous := range issue.bodies {
			if stabilizationScope(previous) != "" {
				continue
			}
			origin, ok := issueorigin.Parse(previous)
			sameJob := origin.Kind == "doctor" && origin.Instance == "github-actions" && strings.HasPrefix(origin.Source, "https://github.com/digitaldrywood/detent/actions/runs/") && scheduledJob(previous) == scheduledJob(body)
			if !ok || !issueorigin.SameDefect(previous, legacy) && !sameJob {
				continue
			}
			fmt.Fprintf(&context, "\nExisting repair: %s (#%d, %s). Preserve its ownership and holds; reconcile this occurrence with that work.\n", issue.item.WorkItemID, issue.item.Number, issue.item.State)
			break
		}
	}
	return context.String()
}
