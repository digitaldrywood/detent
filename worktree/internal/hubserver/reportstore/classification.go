package reportstore

import (
	"slices"
	"strings"
)

const (
	KindInfrastructure = "infrastructure"
	KindIssue          = "issue"
	KindUnknown        = "unknown"
)

type Class struct {
	Name string
	Kind string
}

type classRule struct {
	class    Class
	contains []string
}

var NotClassified = Class{Name: "Not classified", Kind: KindUnknown}

var reworkClassRules = []classRule{
	{Class{"Review findings to address", KindIssue}, []string{"review conversations", "changes requested", "review finding"}},
	{Class{"Landing refused: protected base", KindInfrastructure}, []string{"base protected", "protected base"}},
	{Class{"Gate failed before landing", KindIssue}, []string{"gate failed"}},
	{Class{"Base moved after validation", KindInfrastructure}, []string{"base moved", "base branch changed"}},
	{Class{"Conflict on landing", KindIssue}, []string{"conflict"}},
	{Class{"Finished without a change", KindIssue}, []string{"without work product", "no work product"}},
}

var failureClassRules = []classRule{
	{Class{"Review conversations unresolved at merge", KindIssue}, []string{"review conversations"}},
	{Class{"Landing refused: protected base", KindInfrastructure}, []string{"base protected", "protected base"}},
	{Class{"Conflict on landing", KindIssue}, []string{"landing refused: conflict"}},
	{Class{"Finalization gate failed", KindIssue}, []string{"source finalization gate failed"}},
	{Class{"Merge resolution gate failed", KindIssue}, []string{"merge resolution gate failed"}},
	{Class{"Landing gate failed", KindIssue}, []string{"landing gate failed"}},
	{Class{"Worker processes left running after stop", KindInfrastructure}, []string{"process reap failed", "processes remained after"}},
	{Class{"Hub request timed out", KindInfrastructure}, []string{"hub unavailable", "hub request timed out"}},
	{Class{"Hub request failed", KindInfrastructure}, []string{"hub request failed"}},
	{Class{"Workspace preparation failed", KindInfrastructure}, []string{"workspace preparation failed", "commit squash:"}},
	{Class{"Agent turn interrupted", KindInfrastructure}, []string{"interrupted turn"}},
	{Class{"Agent turn timed out", KindInfrastructure}, []string{"worker cancellation: deadline exceeded", "agent turn: context deadline exceeded"}},
	{Class{"Instance lock held", KindInfrastructure}, []string{"instance lock is held"}},
	{Class{"Finished without a change", KindIssue}, []string{"without work product", "no work product"}},
}

func ClassifyReworkReason(fromState, reason string) Class {
	if fromState == "Human Review" {
		return reworkClassRules[0].class
	}
	return matchReportClass(reworkClassRules, reason)
}

func ClassifyFailure(text string) Class {
	return matchReportClass(failureClassRules, text)
}

func matchReportClass(rules []classRule, text string) Class {
	line := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])), "_", " ")
	for _, rule := range rules {
		if strings.EqualFold(line, strings.ToLower(rule.class.Name)) || slices.ContainsFunc(rule.contains, func(fragment string) bool { return strings.Contains(line, fragment) }) {
			return rule.class
		}
	}
	return NotClassified
}
