package hubserver

import (
	"encoding/json"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func boundBoardProjection(projected tracker.NativeIssue, reporter viewstore.BoardReporterRelations) (board tracker.NativeIssue, list tracker.NativeIssue, boundedReporter viewstore.BoardReporterRelations, listBytes int, err error) {
	list, err = operatortool.BoundNativeListIssueBytes(projected, operatortool.WorkListPageBytes-1024)
	if err != nil {
		return
	}
	board = list
	board.Provenance = projected.Provenance
	board.OmittedFields = slices.DeleteFunc(slices.Clone(board.OmittedFields), func(field string) bool { return field == "provenance" })
	list = operatortool.NativeListIssue(board)
	boundedReporter = reporter
	for _, field := range list.OmittedFields {
		switch field {
		case "dependencies":
			boundedReporter.Dependencies = list.Dependencies
		case "blockers":
			boundedReporter.Blockers = list.Blockers
		case "dependents":
			boundedReporter.Dependents = list.Dependents
		case "related":
			boundedReporter.Related = list.Related
		case "dependency_ready":
			boundedReporter.DependencyReady = list.DependencyReady
		case "blocked_reason":
			boundedReporter.BlockedReason = list.BlockedReason
		case "unblocks_reason":
			boundedReporter.UnblocksReason = list.UnblocksReason
		}
	}
	reporterIssue := list
	reporterIssue.ReporterStatus = boundedReporter.ReporterStatus
	reporterIssue.Dependencies, reporterIssue.Blockers, reporterIssue.Dependents, reporterIssue.Related = boundedReporter.Dependencies, boundedReporter.Blockers, boundedReporter.Dependents, boundedReporter.Related
	reporterIssue.DependencyReady, reporterIssue.BlockedReason, reporterIssue.UnblocksReason = boundedReporter.DependencyReady, boundedReporter.BlockedReason, boundedReporter.UnblocksReason
	for _, issue := range []tracker.NativeIssue{list, reporterIssue} {
		issue = boardIssueCollections(issue)
		var raw []byte
		raw, err = json.Marshal(operatortool.NativeItemPage(string(projected.ProjectID), tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{issue}}))
		if err != nil {
			return
		}
		listBytes = max(listBytes, len(raw))
	}
	return
}
