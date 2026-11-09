package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthIssueScope(organization tracker.OrganizationID, project tracker.ProjectID) nativeScope {
	return nativeScope{organization: organization, project: project, sourceActor: nativeIntegrationActor("health_detector")}
}

func healthIssueFindings(f healthFinding) []healthFinding {
	if f.Signal != "retry_storm" || len(f.Evidence.Signatures) == 0 {
		return []healthFinding{f}
	}
	signatures := slices.Clone(f.Evidence.Signatures)
	slices.Sort(signatures)
	signatures = slices.Compact(signatures)
	findings := make([]healthFinding, 0, len(signatures))
	for _, signature := range signatures {
		finding := f
		sum := sha256.Sum256([]byte("health_intake\x00retry_storm\x00" + signature))
		finding.Fingerprint = hex.EncodeToString(sum[:])
		finding.Evidence.Signatures = []string{signature}
		findings = append(findings, finding)
	}
	return findings
}

func healthIssueRequest(f healthFinding) (tracker.CreateIssue, error) {
	evidence, err := json.MarshalIndent(f.Evidence, "", "  ")
	if err != nil {
		return tracker.CreateIssue{}, err
	}
	title := fmt.Sprintf("%s on %s %s", f.Signal, f.Subject.Kind, f.Subject.ID)
	if f.Signal == "retry_storm" && len(f.Evidence.Signatures) == 1 {
		title = f.Signal + ": " + f.Evidence.Signatures[0]
	}
	request := tracker.CreateIssue{
		Title:    "intake(instance): " + title,
		State:    "Todo",
		Priority: new(1),
		Labels:   []string{"infrastructure"},
	}
	body := fmt.Sprintf(
		"## Current behavior\n\nFinding: `%s`\nSignal: %s\nClass: %s\nSubject: %s %s\nOpened: %s\nObserved: %s\n\n%s\n\nDiagnostic next action: %s\n\nRecorded evidence for this observation window:\n```json\n%s\n```\n\n"+
			"## Desired behavior\n\nClear the recorded `%s` condition for %s %s by correcting its diagnosed cause within existing intended behavior.\n\n"+
			"## Acceptance criteria\n\n- Reproduce the recorded condition with a focused test or fixture using the evidence above, and show that it fails before the repair.\n- Correct the diagnosed cause so that the same test or fixture passes and the recorded condition is cleared in that fixture before merge.\n\n"+
			"## Must not break\n\nPreserve existing behavior, machine fingerprints, duplicate ownership and instance attribution. Infrastructure evidence alone does not authorize speculative source changes; establish the cause with the reproducing test or fixture first.\n\n"+
			"## How we know it worked\n\nRecord the reproducing test or fixture, its command and its failing and passing results against the observed cause. Run focused checks for the changed packages before merge; no deployment or outside-system verification is required.\n\n"+
			"## Invariants touched\n\nPreserve INV-2 instance attribution, INV-7 machine identity, INV-18 bounded writes and INV-21 bounded evaluation cost. Under INV-3, repair or consolidate the existing owner; add no protection mechanism.\n\n"+
			"## Safety regression\n\nThis report changes no safety-critical path. If diagnosis requires changing completion, retry or dispatch controls, preserve their existing safety regression tables and boundary seeds and name the affected behavior in the repair.\n\n```detent-agent\nschema: 1\neffort: high\n```",
		f.ID, f.Signal, f.Class, f.Subject.Kind, f.Subject.ID, formatHubTime(f.OpenedAt), formatHubTime(f.LastSeenAt), f.Summary, f.NextAction, evidence, f.Signal, f.Subject.Kind, f.Subject.ID)
	request.Body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: f.ID, Fingerprint: f.Fingerprint})
	return request, nil
}

func healthIssueDigest(f healthFinding) (string, error) {
	signatures := slices.Clone(f.Evidence.Signatures)
	slices.Sort(signatures)
	signatures = slices.Compact(signatures)
	material := struct {
		Fingerprint string
		Class       string
		Subject     healthSubject
		Signatures  []string `json:",omitempty"`
		Severity    string
	}{f.Fingerprint, f.Class, f.Subject, signatures, f.Severity}
	raw, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
