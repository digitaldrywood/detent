package claimapi

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestTypedAnsweredClaimFacts(t *testing.T) {
	t.Parallel()
	const humanReport = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the plan\n```"
	const permissionReport = "```detent-status\nschema: 1\nstatus: blocked\nreason_code: permission_wait\nblockers: []\nhuman_action: null\n```"
	for _, test := range []struct {
		name, status, reason, effect, state, want, claim, body string
		present, human, blockers, acceptance                   bool
	}{
		{name: "human", present: true, status: "blocked", human: true, want: "human_action", claim: "human_action"},
		{name: "permission", present: true, status: "blocked", reason: "permission_wait", want: "permission_wait", claim: "permission_wait"},
		{name: "recorded acceptance", acceptance: true, want: "recorded_acceptance", claim: "recorded_acceptance"},
		{name: "continuing acceptance", present: true, status: "in_progress", acceptance: true, claim: "progress"},
		{name: "blocked acceptance", present: true, status: "in_progress", blockers: true, acceptance: true, want: "recorded_acceptance", claim: "recorded_acceptance"},
		{name: "legacy push acceptance", effect: "git_push", state: "confirmed", acceptance: true, claim: "progress"},
		{name: "pending human", present: true, status: "blocked", human: true, effect: "git_push", state: "pending", claim: "progress"},
		{name: "ambiguous permission", present: true, status: "blocked", reason: "permission_wait", effect: "pr_create", state: "ambiguous", claim: "progress"},
		{name: "confirmed human", present: true, status: "blocked", human: true, effect: "git_push", state: "confirmed", want: "human_action", claim: "human_action"},
		{name: "plain success"},
		{name: "report-only human", body: humanReport, claim: "human_action"},
		{name: "report-only permission", body: permissionReport, claim: "permission_wait"},
		{name: "malformed report", body: "```detent-status\nschema: 1\nstatus: blocked\nhuman_action: [\n```"},
		{name: "typed complete overrides report", present: true, status: workpad.StatusComplete, body: humanReport},
		{name: "pending report-only human", body: humanReport, effect: "git_push", state: "pending", claim: "progress"},
		{name: "instance limitation", present: true, status: "blocked", reason: "instance_limitation", claim: "progress"},
		{name: "untyped blocker", present: true, status: "blocked", blockers: true, claim: "progress"},
		{name: "complete disposition", present: true, status: workpad.StatusComplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tracker.NativeRunData{CompletionBody: test.body, Runtime: &tracker.NativeRuntimeObservation{Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: test.acceptance}}}
			if test.present {
				data.Disposition = &tracker.NativeDisposition{Status: test.status, HumanAction: test.human, Blockers: test.blockers, ReasonCode: test.reason}
			}
			checkpoint := &tracker.NativeCheckpoint{ExternalEffect: test.effect, EffectState: test.state}
			data.Handoff = checkpoint
			if got := AnsweredClaimKind(data, checkpoint); got != test.want {
				t.Fatalf("classification=%q want %q", got, test.want)
			}
			if got := NativeAttemptClaimDisposition(data, checkpoint, "prj_shared"); got != test.claim {
				t.Fatalf("claim disposition=%q want %q", got, test.claim)
			}
		})
	}
}
