package postgres

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (m *boardPageModel) restore() {

	if m.linkedCommentsBody != nil {
		m.issue.LinkedSource.Snapshot.Comments = make([]tracker.GitHubIssueComment, len(m.linkedCommentsBody))
		for i := range m.issue.LinkedSource.Snapshot.Comments {
			m.issue.LinkedSource.Snapshot.Comments[i].Body = m.linkedCommentsBody[i]
			m.issue.LinkedSource.Snapshot.Comments[i].Provenance.Provider = m.linkedCommentsProvenanceProvider[i]
			m.issue.LinkedSource.Snapshot.Comments[i].Provenance.ExternalID = m.linkedCommentsProvenanceExternalID[i]
			m.issue.LinkedSource.Snapshot.Comments[i].Provenance.AuthorID = m.linkedCommentsProvenanceAuthorID[i]
			m.issue.LinkedSource.Snapshot.Comments[i].Provenance.AuthorDisplayName = m.linkedCommentsProvenanceAuthorDisplayName[i]
			m.issue.LinkedSource.Snapshot.Comments[i].Provenance.CreatedAt = m.linkedCommentsProvenanceCreatedAt[i]
			m.issue.LinkedSource.Snapshot.Comments[i].Provenance.UpdatedAt = m.linkedCommentsProvenanceUpdatedAt[i]
			m.issue.LinkedSource.Snapshot.Comments[i].Provenance.ObservedAt = m.linkedCommentsProvenanceObservedAt[i]
		}
	}
	if m.blockersDispatchable != nil {
		m.issue.Blockers = make([]tracker.NativeDependency, len(m.blockersDispatchable))
		for i := range m.issue.Blockers {
			m.issue.Blockers[i].Dispatchable = m.blockersDispatchable[i]
			m.issue.Blockers[i].ClosedAt = m.blockersClosedAt[i]
			m.issue.Blockers[i].Number = m.blockersNumber[i]
			m.issue.Blockers[i].Title = m.blockersTitle[i]
			m.issue.Blockers[i].WebURL = m.blockersWebURL[i]
			m.issue.Blockers[i].StartedAt = m.blockersStartedAt[i]
			m.issue.Blockers[i].Identifier = m.blockersIDentifier[i]
			m.issue.Blockers[i].ID = m.blockersID[i]
			m.issue.Blockers[i].ProjectID = m.blockersProjectID[i]
			m.issue.Blockers[i].State = m.blockersState[i]
			m.issue.Blockers[i].Terminal = m.blockersTerminal[i]
		}
	}
	if m.dependentsDispatchable != nil {
		m.issue.Dependents = make([]tracker.NativeDependency, len(m.dependentsDispatchable))
		for i := range m.issue.Dependents {
			m.issue.Dependents[i].Dispatchable = m.dependentsDispatchable[i]
			m.issue.Dependents[i].ClosedAt = m.dependentsClosedAt[i]
			m.issue.Dependents[i].Number = m.dependentsNumber[i]
			m.issue.Dependents[i].Title = m.dependentsTitle[i]
			m.issue.Dependents[i].WebURL = m.dependentsWebURL[i]
			m.issue.Dependents[i].StartedAt = m.dependentsStartedAt[i]
			m.issue.Dependents[i].Identifier = m.dependentsIDentifier[i]
			m.issue.Dependents[i].ID = m.dependentsID[i]
			m.issue.Dependents[i].ProjectID = m.dependentsProjectID[i]
			m.issue.Dependents[i].State = m.dependentsState[i]
			m.issue.Dependents[i].Terminal = m.dependentsTerminal[i]
		}
	}
	if m.relatedDispatchable != nil {
		m.issue.Related = make([]tracker.NativeDependency, len(m.relatedDispatchable))
		for i := range m.issue.Related {
			m.issue.Related[i].Dispatchable = m.relatedDispatchable[i]
			m.issue.Related[i].ClosedAt = m.relatedClosedAt[i]
			m.issue.Related[i].Number = m.relatedNumber[i]
			m.issue.Related[i].Title = m.relatedTitle[i]
			m.issue.Related[i].WebURL = m.relatedWebURL[i]
			m.issue.Related[i].StartedAt = m.relatedStartedAt[i]
			m.issue.Related[i].Identifier = m.relatedIDentifier[i]
			m.issue.Related[i].ID = m.relatedID[i]
			m.issue.Related[i].ProjectID = m.relatedProjectID[i]
			m.issue.Related[i].State = m.relatedState[i]
			m.issue.Related[i].Terminal = m.relatedTerminal[i]
		}
	}
	if m.externalReferencesProvider != nil {
		m.issue.ExternalReferences = make([]tracker.ExternalReference, len(m.externalReferencesProvider))
		for i := range m.issue.ExternalReferences {
			m.issue.ExternalReferences[i].Provider = m.externalReferencesProvider[i]
			m.issue.ExternalReferences[i].Kind = m.externalReferencesKind[i]
			m.issue.ExternalReferences[i].ID = m.externalReferencesID[i]
			m.issue.ExternalReferences[i].URL = m.externalReferencesURL[i]
			m.issue.ExternalReferences[i].Repository = m.externalReferencesRepository[i]
			m.issue.ExternalReferences[i].Number = m.externalReferencesNumber[i]
		}
	}
	if m.reporterBlockersDispatchable != nil {
		m.reporter.Blockers = make([]tracker.NativeDependency, len(m.reporterBlockersDispatchable))
		for i := range m.reporter.Blockers {
			m.reporter.Blockers[i].Dispatchable = m.reporterBlockersDispatchable[i]
			m.reporter.Blockers[i].ClosedAt = m.reporterBlockersClosedAt[i]
			m.reporter.Blockers[i].Number = m.reporterBlockersNumber[i]
			m.reporter.Blockers[i].Title = m.reporterBlockersTitle[i]
			m.reporter.Blockers[i].WebURL = m.reporterBlockersWebURL[i]
			m.reporter.Blockers[i].StartedAt = m.reporterBlockersStartedAt[i]
			m.reporter.Blockers[i].Identifier = m.reporterBlockersIDentifier[i]
			m.reporter.Blockers[i].ID = m.reporterBlockersID[i]
			m.reporter.Blockers[i].ProjectID = m.reporterBlockersProjectID[i]
			m.reporter.Blockers[i].State = m.reporterBlockersState[i]
			m.reporter.Blockers[i].Terminal = m.reporterBlockersTerminal[i]
		}
	}
	if m.reporterDependentsDispatchable != nil {
		m.reporter.Dependents = make([]tracker.NativeDependency, len(m.reporterDependentsDispatchable))
		for i := range m.reporter.Dependents {
			m.reporter.Dependents[i].Dispatchable = m.reporterDependentsDispatchable[i]
			m.reporter.Dependents[i].ClosedAt = m.reporterDependentsClosedAt[i]
			m.reporter.Dependents[i].Number = m.reporterDependentsNumber[i]
			m.reporter.Dependents[i].Title = m.reporterDependentsTitle[i]
			m.reporter.Dependents[i].WebURL = m.reporterDependentsWebURL[i]
			m.reporter.Dependents[i].StartedAt = m.reporterDependentsStartedAt[i]
			m.reporter.Dependents[i].Identifier = m.reporterDependentsIDentifier[i]
			m.reporter.Dependents[i].ID = m.reporterDependentsID[i]
			m.reporter.Dependents[i].ProjectID = m.reporterDependentsProjectID[i]
			m.reporter.Dependents[i].State = m.reporterDependentsState[i]
			m.reporter.Dependents[i].Terminal = m.reporterDependentsTerminal[i]
		}
	}
	if m.reporterRelatedDispatchable != nil {
		m.reporter.Related = make([]tracker.NativeDependency, len(m.reporterRelatedDispatchable))
		for i := range m.reporter.Related {
			m.reporter.Related[i].Dispatchable = m.reporterRelatedDispatchable[i]
			m.reporter.Related[i].ClosedAt = m.reporterRelatedClosedAt[i]
			m.reporter.Related[i].Number = m.reporterRelatedNumber[i]
			m.reporter.Related[i].Title = m.reporterRelatedTitle[i]
			m.reporter.Related[i].WebURL = m.reporterRelatedWebURL[i]
			m.reporter.Related[i].StartedAt = m.reporterRelatedStartedAt[i]
			m.reporter.Related[i].Identifier = m.reporterRelatedIDentifier[i]
			m.reporter.Related[i].ID = m.reporterRelatedID[i]
			m.reporter.Related[i].ProjectID = m.reporterRelatedProjectID[i]
			m.reporter.Related[i].State = m.reporterRelatedState[i]
			m.reporter.Related[i].Terminal = m.reporterRelatedTerminal[i]
		}
	}
	if m.contractSectionsKeys != nil {
		m.issue.IssueContract.ConfirmedSections = make(map[string]string, len(m.contractSectionsKeys))
		for i, key := range m.contractSectionsKeys {
			m.issue.IssueContract.ConfirmedSections[key] = m.contractSectionsValues[i]
		}
	}
	if !m.reporterRelationsPresent {
		m.reporter = nil
	}
	if !m.changePresent {
		m.issue.Change = nil
	}
	if !m.pullRequestPresent {
		m.issue.PullRequest = nil
	}
	if !m.listChangePresent {
		m.issue.ListChange = nil
	}
	if !m.provenancePresent {
		m.issue.Provenance = nil
	}
	if !m.verificationVerifiedByPresent {
		m.issue.Verification.VerifiedBy = nil
	}
	if !m.verificationPresent {
		m.issue.Verification = nil
	}
	if !m.reportContextPresent {
		m.issue.ReportContext = nil
	}
	if !m.linkedSnapshotPresent {
		m.issue.LinkedSource.Snapshot = nil
	}
	if !m.linkedSourcePresent {
		m.issue.LinkedSource = nil
	}
	if !m.issueContractReleaseDeliveryPresent {
		m.issue.IssueContract.ReleaseDelivery = nil
	}
	if !m.issueContractPresent {
		m.issue.IssueContract = nil
	}
	if !m.releaseDeliveryPresent {
		m.issue.ReleaseDelivery = nil
	}
	if !m.ready {
		m.issue = nil
	}
}
