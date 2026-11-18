package postgres

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardPageModel struct {
	issue                                     *tracker.NativeIssue
	reporter                                  *viewstore.BoardReporterRelations
	ready                                     bool
	projectedAt                               time.Time
	releaseDeliveryPresent                    bool
	issueContractPresent                      bool
	issueContractReleaseDeliveryPresent       bool
	contractSectionsKeys                      []string
	contractSectionsValues                    []string
	linkedSourcePresent                       bool
	linkedSnapshotPresent                     bool
	linkedCommentsBody                        []string
	linkedCommentsProvenanceProvider          []string
	linkedCommentsProvenanceExternalID        []string
	linkedCommentsProvenanceAuthorID          []string
	linkedCommentsProvenanceAuthorDisplayName []string
	linkedCommentsProvenanceCreatedAt         []time.Time
	linkedCommentsProvenanceUpdatedAt         []time.Time
	linkedCommentsProvenanceObservedAt        []time.Time
	reportContextPresent                      bool
	verificationPresent                       bool
	verificationVerifiedByPresent             bool
	provenancePresent                         bool
	listChangePresent                         bool
	pullRequestPresent                        bool
	blockersDispatchable                      []bool
	blockersClosedAt                          []*time.Time
	blockersNumber                            []int
	blockersTitle                             []string
	blockersWebURL                            []string
	blockersStartedAt                         []*time.Time
	blockersIDentifier                        []string
	blockersID                                []tracker.NativeWorkItemID
	blockersProjectID                         []tracker.ProjectID
	blockersState                             []string
	blockersTerminal                          []bool
	dependentsDispatchable                    []bool
	dependentsClosedAt                        []*time.Time
	dependentsNumber                          []int
	dependentsTitle                           []string
	dependentsWebURL                          []string
	dependentsStartedAt                       []*time.Time
	dependentsIDentifier                      []string
	dependentsID                              []tracker.NativeWorkItemID
	dependentsProjectID                       []tracker.ProjectID
	dependentsState                           []string
	dependentsTerminal                        []bool
	relatedDispatchable                       []bool
	relatedClosedAt                           []*time.Time
	relatedNumber                             []int
	relatedTitle                              []string
	relatedWebURL                             []string
	relatedStartedAt                          []*time.Time
	relatedIDentifier                         []string
	relatedID                                 []tracker.NativeWorkItemID
	relatedProjectID                          []tracker.ProjectID
	relatedState                              []string
	relatedTerminal                           []bool
	externalReferencesProvider                []string
	externalReferencesKind                    []string
	externalReferencesID                      []string
	externalReferencesURL                     []string
	externalReferencesRepository              []string
	externalReferencesNumber                  []int
	changePresent                             bool
	reporterRelationsPresent                  bool
	reporterBlockersDispatchable              []bool
	reporterBlockersClosedAt                  []*time.Time
	reporterBlockersNumber                    []int
	reporterBlockersTitle                     []string
	reporterBlockersWebURL                    []string
	reporterBlockersStartedAt                 []*time.Time
	reporterBlockersIDentifier                []string
	reporterBlockersID                        []tracker.NativeWorkItemID
	reporterBlockersProjectID                 []tracker.ProjectID
	reporterBlockersState                     []string
	reporterBlockersTerminal                  []bool
	reporterDependentsDispatchable            []bool
	reporterDependentsClosedAt                []*time.Time
	reporterDependentsNumber                  []int
	reporterDependentsTitle                   []string
	reporterDependentsWebURL                  []string
	reporterDependentsStartedAt               []*time.Time
	reporterDependentsIDentifier              []string
	reporterDependentsID                      []tracker.NativeWorkItemID
	reporterDependentsProjectID               []tracker.ProjectID
	reporterDependentsState                   []string
	reporterDependentsTerminal                []bool
	reporterRelatedDispatchable               []bool
	reporterRelatedClosedAt                   []*time.Time
	reporterRelatedNumber                     []int
	reporterRelatedTitle                      []string
	reporterRelatedWebURL                     []string
	reporterRelatedStartedAt                  []*time.Time
	reporterRelatedIDentifier                 []string
	reporterRelatedID                         []tracker.NativeWorkItemID
	reporterRelatedProjectID                  []tracker.ProjectID
	reporterRelatedState                      []string
	reporterRelatedTerminal                   []bool
}

func boardPageVector[T, U any](values []T, field func(T) U) []U {
	if values == nil {
		return nil
	}
	result := make([]U, len(values))
	for i, value := range values {
		result[i] = field(value)
	}
	return result
}

func boardPageTargets(b *viewstore.BoardRecord) ([]any, func()) {
	m := newBoardPageModel(nil, nil)
	return m.targets(), func() {
		m.restore()
		b.ProjectedAt = m.projectedAt
		b.PageIssue = m.issue
		b.ReporterRelations = m.reporter
	}
}
