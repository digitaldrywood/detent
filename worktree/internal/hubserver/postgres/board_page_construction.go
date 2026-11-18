package postgres

import (
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newBoardPageModel(issue *tracker.NativeIssue, reporter *viewstore.BoardReporterRelations) *boardPageModel {
	m := &boardPageModel{issue: issue, reporter: reporter, ready: issue != nil}
	if m.issue != nil {
		copy := *m.issue
		m.issue = &copy
		m.ready = true
	} else {
		m.issue = &tracker.NativeIssue{}
	}
	if m.issue.ReleaseDelivery != nil {
		copy := *m.issue.ReleaseDelivery
		m.issue.ReleaseDelivery = &copy
		m.releaseDeliveryPresent = true
	} else {
		m.issue.ReleaseDelivery = &tracker.ReleaseDelivery{}
	}
	if m.issue.IssueContract != nil {
		copy := *m.issue.IssueContract
		m.issue.IssueContract = &copy
		m.issueContractPresent = true
	} else {
		m.issue.IssueContract = &issuecontract.State{}
	}
	if m.issue.IssueContract.ReleaseDelivery != nil {
		copy := *m.issue.IssueContract.ReleaseDelivery
		m.issue.IssueContract.ReleaseDelivery = &copy
		m.issueContractReleaseDeliveryPresent = true
	} else {
		m.issue.IssueContract.ReleaseDelivery = &issuecontract.ReleaseDeliveryApproval{}
	}
	if m.issue.LinkedSource != nil {
		copy := *m.issue.LinkedSource
		m.issue.LinkedSource = &copy
		m.linkedSourcePresent = true
	} else {
		m.issue.LinkedSource = &tracker.LinkedIssueSource{}
	}
	if m.issue.LinkedSource.Snapshot != nil {
		copy := *m.issue.LinkedSource.Snapshot
		m.issue.LinkedSource.Snapshot = &copy
		m.linkedSnapshotPresent = true
	} else {
		m.issue.LinkedSource.Snapshot = &tracker.GitHubIssueSnapshot{}
	}
	if m.issue.ReportContext != nil {
		copy := *m.issue.ReportContext
		m.issue.ReportContext = &copy
		m.reportContextPresent = true
	} else {
		m.issue.ReportContext = &tracker.ReportContext{}
	}
	if m.issue.Verification != nil {
		copy := *m.issue.Verification
		m.issue.Verification = &copy
		m.verificationPresent = true
	} else {
		m.issue.Verification = &tracker.NativeVerification{}
	}
	if m.issue.Verification.VerifiedBy != nil {
		copy := *m.issue.Verification.VerifiedBy
		m.issue.Verification.VerifiedBy = &copy
		m.verificationVerifiedByPresent = true
	} else {
		m.issue.Verification.VerifiedBy = &tracker.Actor{}
	}
	if m.issue.Provenance != nil {
		copy := *m.issue.Provenance
		m.issue.Provenance = &copy
		m.provenancePresent = true
	} else {
		m.issue.Provenance = &tracker.Provenance{}
	}
	if m.issue.ListChange != nil {
		copy := *m.issue.ListChange
		m.issue.ListChange = &copy
		m.listChangePresent = true
	} else {
		m.issue.ListChange = &tracker.NativeListChange{}
	}
	if m.issue.PullRequest != nil {
		copy := *m.issue.PullRequest
		m.issue.PullRequest = &copy
		m.pullRequestPresent = true
	} else {
		m.issue.PullRequest = &tracker.NativePullRequest{}
	}
	if m.issue.Change != nil {
		copy := *m.issue.Change
		m.issue.Change = &copy
		m.changePresent = true
	} else {
		m.issue.Change = &tracker.NativeIssueChange{}
	}
	if m.reporter != nil {
		copy := *m.reporter
		m.reporter = &copy
		m.reporterRelationsPresent = true
	} else {
		m.reporter = &viewstore.BoardReporterRelations{}
	}
	m.linkedCommentsBody = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) string { return v.Body })
	m.linkedCommentsProvenanceProvider = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) string { return v.Provenance.Provider })
	m.linkedCommentsProvenanceExternalID = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) string { return v.Provenance.ExternalID })
	m.linkedCommentsProvenanceAuthorID = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) string { return v.Provenance.AuthorID })
	m.linkedCommentsProvenanceAuthorDisplayName = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) string { return v.Provenance.AuthorDisplayName })
	m.linkedCommentsProvenanceCreatedAt = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) time.Time { return v.Provenance.CreatedAt })
	m.linkedCommentsProvenanceUpdatedAt = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) time.Time { return v.Provenance.UpdatedAt })
	m.linkedCommentsProvenanceObservedAt = boardPageVector(m.issue.LinkedSource.Snapshot.Comments, func(v tracker.GitHubIssueComment) time.Time { return v.Provenance.ObservedAt })
	m.blockersDispatchable = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) bool { return v.Dispatchable })
	m.blockersClosedAt = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) *time.Time { return v.ClosedAt })
	m.blockersNumber = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) int { return v.Number })
	m.blockersTitle = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) string { return v.Title })
	m.blockersWebURL = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) string { return v.WebURL })
	m.blockersStartedAt = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) *time.Time { return v.StartedAt })
	m.blockersIDentifier = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) string { return v.Identifier })
	m.blockersID = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) tracker.NativeWorkItemID { return v.ID })
	m.blockersProjectID = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) tracker.ProjectID { return v.ProjectID })
	m.blockersState = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) string { return v.State })
	m.blockersTerminal = boardPageVector(m.issue.Blockers, func(v tracker.NativeDependency) bool { return v.Terminal })
	m.dependentsDispatchable = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) bool { return v.Dispatchable })
	m.dependentsClosedAt = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) *time.Time { return v.ClosedAt })
	m.dependentsNumber = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) int { return v.Number })
	m.dependentsTitle = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) string { return v.Title })
	m.dependentsWebURL = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) string { return v.WebURL })
	m.dependentsStartedAt = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) *time.Time { return v.StartedAt })
	m.dependentsIDentifier = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) string { return v.Identifier })
	m.dependentsID = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) tracker.NativeWorkItemID { return v.ID })
	m.dependentsProjectID = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) tracker.ProjectID { return v.ProjectID })
	m.dependentsState = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) string { return v.State })
	m.dependentsTerminal = boardPageVector(m.issue.Dependents, func(v tracker.NativeDependency) bool { return v.Terminal })
	m.relatedDispatchable = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) bool { return v.Dispatchable })
	m.relatedClosedAt = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) *time.Time { return v.ClosedAt })
	m.relatedNumber = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) int { return v.Number })
	m.relatedTitle = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) string { return v.Title })
	m.relatedWebURL = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) string { return v.WebURL })
	m.relatedStartedAt = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) *time.Time { return v.StartedAt })
	m.relatedIDentifier = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) string { return v.Identifier })
	m.relatedID = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) tracker.NativeWorkItemID { return v.ID })
	m.relatedProjectID = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) tracker.ProjectID { return v.ProjectID })
	m.relatedState = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) string { return v.State })
	m.relatedTerminal = boardPageVector(m.issue.Related, func(v tracker.NativeDependency) bool { return v.Terminal })
	m.externalReferencesProvider = boardPageVector(m.issue.ExternalReferences, func(v tracker.ExternalReference) string { return v.Provider })
	m.externalReferencesKind = boardPageVector(m.issue.ExternalReferences, func(v tracker.ExternalReference) string { return v.Kind })
	m.externalReferencesID = boardPageVector(m.issue.ExternalReferences, func(v tracker.ExternalReference) string { return v.ID })
	m.externalReferencesURL = boardPageVector(m.issue.ExternalReferences, func(v tracker.ExternalReference) string { return v.URL })
	m.externalReferencesRepository = boardPageVector(m.issue.ExternalReferences, func(v tracker.ExternalReference) string { return v.Repository })
	m.externalReferencesNumber = boardPageVector(m.issue.ExternalReferences, func(v tracker.ExternalReference) int { return v.Number })
	m.reporterBlockersDispatchable = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) bool { return v.Dispatchable })
	m.reporterBlockersClosedAt = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) *time.Time { return v.ClosedAt })
	m.reporterBlockersNumber = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) int { return v.Number })
	m.reporterBlockersTitle = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) string { return v.Title })
	m.reporterBlockersWebURL = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) string { return v.WebURL })
	m.reporterBlockersStartedAt = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) *time.Time { return v.StartedAt })
	m.reporterBlockersIDentifier = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) string { return v.Identifier })
	m.reporterBlockersID = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) tracker.NativeWorkItemID { return v.ID })
	m.reporterBlockersProjectID = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) tracker.ProjectID { return v.ProjectID })
	m.reporterBlockersState = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) string { return v.State })
	m.reporterBlockersTerminal = boardPageVector(m.reporter.Blockers, func(v tracker.NativeDependency) bool { return v.Terminal })
	m.reporterDependentsDispatchable = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) bool { return v.Dispatchable })
	m.reporterDependentsClosedAt = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) *time.Time { return v.ClosedAt })
	m.reporterDependentsNumber = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) int { return v.Number })
	m.reporterDependentsTitle = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) string { return v.Title })
	m.reporterDependentsWebURL = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) string { return v.WebURL })
	m.reporterDependentsStartedAt = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) *time.Time { return v.StartedAt })
	m.reporterDependentsIDentifier = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) string { return v.Identifier })
	m.reporterDependentsID = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) tracker.NativeWorkItemID { return v.ID })
	m.reporterDependentsProjectID = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) tracker.ProjectID { return v.ProjectID })
	m.reporterDependentsState = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) string { return v.State })
	m.reporterDependentsTerminal = boardPageVector(m.reporter.Dependents, func(v tracker.NativeDependency) bool { return v.Terminal })
	m.reporterRelatedDispatchable = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) bool { return v.Dispatchable })
	m.reporterRelatedClosedAt = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) *time.Time { return v.ClosedAt })
	m.reporterRelatedNumber = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) int { return v.Number })
	m.reporterRelatedTitle = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) string { return v.Title })
	m.reporterRelatedWebURL = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) string { return v.WebURL })
	m.reporterRelatedStartedAt = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) *time.Time { return v.StartedAt })
	m.reporterRelatedIDentifier = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) string { return v.Identifier })
	m.reporterRelatedID = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) tracker.NativeWorkItemID { return v.ID })
	m.reporterRelatedProjectID = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) tracker.ProjectID { return v.ProjectID })
	m.reporterRelatedState = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) string { return v.State })
	m.reporterRelatedTerminal = boardPageVector(m.reporter.Related, func(v tracker.NativeDependency) bool { return v.Terminal })
	if m.issue.IssueContract.ConfirmedSections != nil {
		m.contractSectionsKeys = make([]string, 0, len(m.issue.IssueContract.ConfirmedSections))
		for key := range m.issue.IssueContract.ConfirmedSections {
			m.contractSectionsKeys = append(m.contractSectionsKeys, key)
		}
		sort.Strings(m.contractSectionsKeys)
		m.contractSectionsValues = make([]string, len(m.contractSectionsKeys))
		for i, key := range m.contractSectionsKeys {
			m.contractSectionsValues[i] = m.issue.IssueContract.ConfirmedSections[key]
		}
	}
	return m
}
