package cli

import (
	"context"
	"fmt"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const doctorOperationalCompletionSampleLimit = 5

func checkDoctorOperationalCompletion(
	ctx context.Context,
	id string,
	cfg workflowconfig.Config,
	workflowPrompt string,
	deps doctorDeps,
) doctorCheck {
	name := "Project " + id + " operational completion"
	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("create operational completion diagnostic connector: %v", err), Hint: "Fix tracker credentials and rerun detent doctor."}
	}
	if projectConnector == nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "create operational completion diagnostic connector: connector is nil", Hint: "Fix tracker configuration and rerun detent doctor."}
	}

	states := append(append([]string(nil), cfg.Tracker.ActiveStates...), cfg.Tracker.ObservedStates...)
	issues, fetchErr := projectConnector.FetchIssuesByStates(ctx, states)
	closeErr := closeDoctorAutoPromoteConnector(projectConnector)
	if fetchErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("fetch operational completion authorizations: %v", fetchErr), Hint: "Fix tracker connectivity and rerun detent doctor."}
	}

	authorized := make([]string, 0)
	for _, issue := range issues {
		kind, found, parseErr := workpad.CompletionAuthorizationFromIssueBody(issue.Description)
		if parseErr != nil || !found || kind != workpad.CompletionOperational {
			continue
		}
		identifier := strings.TrimSpace(issue.Identifier)
		if identifier == "" {
			identifier = strings.TrimSpace(issue.ID)
		}
		if identifier != "" {
			authorized = append(authorized, identifier)
		}
	}

	check := doctorCheck{Name: name, Status: doctorOK}
	switch {
	case len(authorized) == 0:
		check.Detail = "no active or observed issue authorizes operational completion"
	case doctorWorkflowMentionsOperationalCompletion(workflowPrompt):
		check.Detail = fmt.Sprintf("workflow documents the operational completion contract used by %d issue(s)", len(authorized))
	default:
		check.Status = doctorWarn
		check.Detail = fmt.Sprintf("%d issue(s) authorize operational completion but the workflow does not document the detent-completion contract: %s", len(authorized), strings.Join(firstDoctorOperationalCompletionIdentifiers(authorized), ", "))
		check.Hint = "Document the issue-body detent-completion authorization and the matching Workpad completion_kind: operational declaration in WORKFLOW.md."
	}
	if closeErr != nil {
		check.Status = doctorWarn
		check.Detail += "; connector close failed: " + closeErr.Error()
		if check.Hint == "" {
			check.Hint = "Rerun detent doctor and check local network resources."
		}
	}
	return check
}

func doctorWorkflowMentionsOperationalCompletion(prompt string) bool {
	prompt = strings.ToLower(prompt)
	return strings.Contains(prompt, "detent-completion") && strings.Contains(prompt, "completion_kind: operational")
}

func firstDoctorOperationalCompletionIdentifiers(identifiers []string) []string {
	if len(identifiers) <= doctorOperationalCompletionSampleLimit {
		return identifiers
	}
	return identifiers[:doctorOperationalCompletionSampleLimit]
}
