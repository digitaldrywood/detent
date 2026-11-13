package githubreconcile

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/githubwebhook"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/pullrequests"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Integration struct {
	Repository string
	Apply      func(context.Context, tracker.NativeWorkItemID, string, tracker.GitHubIntake) error
}
type Deps struct {
	Repository   tenantstore.Repository
	Organization tracker.OrganizationID
	Backend      githubcontract.ReconcileBackend
	Importer     githubcontract.ImportBackend
	Ready        bool
	Unavailable  error
	Now          func() time.Time
	Jitter       func() float64
	Interval     time.Duration
	Integration  func(context.Context, tracker.OrganizationID, tracker.ProjectID) (Integration, error)
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) Maintain(ctx context.Context) error {
	if s.deps.Backend == nil {
		return nil
	}
	if s.deps.Repository == nil || !s.deps.Ready {
		return s.deps.Unavailable
	}
	pullErr := s.MaintainPullRequests(ctx)
	issueErr := s.maintainLinkedIssues(ctx)
	return errors.Join(pullErr, issueErr)
}

func (s *Service) maintainLinkedIssues(ctx context.Context) error {
	if s.deps.Backend == nil || s.deps.Importer == nil {
		return nil
	}
	if s.deps.Repository == nil || !s.deps.Ready {
		return s.deps.Unavailable
	}
	organization := s.deps.Organization
	var row projectstore.GitHubReconciliation
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubReconciliationReader, ok := r.(projectstore.GitHubReconciliationReader)
		if !ok {
			return s.deps.Unavailable
		}
		row, err = gitHubReconciliationReader.NextGitHubReconciliation(ctx, s.deps.Now())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	scope, err := s.deps.Integration(ctx, organization, row.ProjectID)
	if err == nil && !strings.EqualFold(scope.Repository, row.Repository) {
		err = errors.New("GitHub reconciliation repository changed")
	}
	owner, name, _ := githubwebhook.SplitRepositoryFullName(row.Repository)
	var snapshot githubcontract.ReconcileSnapshot
	if err == nil {
		snapshot, err = s.deps.Backend.Reconcile(ctx, githubcontract.ReconcileRequest{Profile: "native", Repository: githubcontract.RepositoryTarget{Owner: owner, Name: name, NodeID: row.NodeID}, SkipIssues: true, SkipRepository: true, Through: s.deps.Now()})
	}
	if err == nil && (snapshot.Repository.NodeID == "" || row.NodeID != "" && snapshot.Repository.NodeID != row.NodeID || !strings.EqualFold(snapshot.Repository.Owner+"/"+snapshot.Repository.Name, row.Repository)) {
		err = errors.New("GitHub reconciliation source identity changed")
	}
	if err == nil {
		row.NodeID = snapshot.Repository.NodeID
	}
	var linked eventstore.LinkedSource
	sweepComplete := false
	if err == nil {
		err = s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			var err error
			gitHubReconciliationReader, ok := r.(projectstore.GitHubReconciliationReader)
			if !ok {
				return s.deps.Unavailable
			}
			linked, err = gitHubReconciliationReader.NextGitHubLinkedSource(ctx, row.ProjectID, row.AfterWorkItem)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			row.AfterWorkItem = ""
			sweepComplete = true
			err = nil
		} else if err == nil {
			_, repository, number, parseErr := tracker.ParseGitHubIssueURL(linked.URL)
			err = parseErr
			if err == nil && !strings.EqualFold(repository, row.Repository) {
				err = errors.New("GitHub linked source repository changed")
			}
			if err == nil {
				page, fetchErr := s.deps.Importer.FetchImportPage(ctx, githubcontract.GitHubImportRequest{Profile: "native", Repository: repository, IssueNumber: number, Stage: "issue"})
				err = fetchErr
				if err == nil && (page.Issue == nil || page.Issue.Number != number) {
					err = errors.New("GitHub reconciliation source is incomplete")
				}
				if err == nil {
					source := githubwebhook.SnapshotFromSource(*page.Issue, s.deps.Now())
					key, hashErr := hubapi.CommandHash(source)
					err = hashErr
					if err == nil {
						err = scope.Apply(ctx, linked.WorkItemID, "github.reconcile", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: key}, Snapshot: source})
					}
				}
			}
			if err == nil {
				row.AfterWorkItem = linked.WorkItemID
			}
		}
	}
	observedAt := s.deps.Now().UTC()
	row.NextAttemptAt = observedAt.Add(time.Second)
	if err != nil {
		row.LastFailureAt = &observedAt
		row.Attempts++
		row.NextAttemptAt = s.retryAt(observedAt, row.Attempts)
	} else {
		row.Attempts = 0
		if sweepComplete {
			row.LastSuccessfulSweepAt = &observedAt
			row.NextAttemptAt = observedAt.Add(time.Duration(float64(s.deps.Interval) * (0.5 + s.deps.Jitter())))
		}
	}
	writeErr := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubReconciliationWriter, ok := w.(projectstore.GitHubReconciliationWriter)
		if !ok {
			return s.deps.Unavailable
		}
		return gitHubReconciliationWriter.SaveGitHubReconciliation(ctx, row)
	})
	return errors.Join(err, writeErr)
}

func (s *Service) retryAt(now time.Time, attempts int64) time.Time {
	return now.Add(time.Duration(float64(time.Second*time.Duration(1<<min(attempts, 8))) * (0.5 + s.deps.Jitter())))
}

func (s *Service) refreshOrigin(ctx context.Context, r tenantstore.Reader, row projectstore.GitHubPullRequestRefresh) (bool, bool, error) {
	project, err := r.ProjectRecord(ctx, row.ProjectID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !project.Settings.RepositoryEnabled || !strings.EqualFold(project.Settings.CheckoutRepository, row.Repository) {
		return false, false, nil
	}
	change, err := r.Change(ctx, row.ChangeID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if change.ProjectID != row.ProjectID {
		return false, false, nil
	}
	if change.CurrentVersionID == nil || *change.CurrentVersionID != row.RequestedVersion {
		return true, false, nil
	}
	version, err := r.Version(ctx, row.ChangeID, row.RequestedVersion)
	if err != nil {
		return true, false, err
	}
	return true, version.HeadSHA == row.RequestedHeadSHA, nil
}

func (s *Service) finishFailure(ctx context.Context, organization tracker.OrganizationID, row projectstore.GitHubPullRequestRefresh, primary error) error {
	err := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(projectstore.GitHubPullRequestRefreshWriter)
		if !ok {
			return s.deps.Unavailable
		}
		live, current, err := s.refreshOrigin(ctx, w, row)
		if err != nil {
			return err
		}
		if !live {
			err = writer.CancelGitHubPullRequestRefresh(ctx, row)
		} else if !current || githubcontract.IsPermanent(primary) {
			err = writer.SupersedeGitHubPullRequestRefresh(ctx, row)
		} else {
			err = writer.RetryGitHubPullRequestRefresh(ctx, row, s.retryAt(s.deps.Now().UTC(), row.Attempts+1))
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nil
		}
		return err
	})
	return errors.Join(primary, err)
}

func (s *Service) MaintainPullRequests(ctx context.Context) error {
	organization := s.deps.Organization
	var row projectstore.GitHubPullRequestRefresh
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		reader, ok := r.(projectstore.GitHubPullRequestRefreshReader)
		if !ok {
			return s.deps.Unavailable
		}
		var err error
		row, err = reader.NextGitHubPullRequestRefresh(ctx, s.deps.Now())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var live, current bool
	var repositoryNode string
	err = s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		live, current, err = s.refreshOrigin(ctx, r, row)
		if err != nil || !live || !current {
			return err
		}
		reader, ok := r.(projectstore.GitHubReconciliationReader)
		if !ok {
			return s.deps.Unavailable
		}
		reconciliation, err := reader.GitHubReconciliation(ctx, row.ProjectID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.EqualFold(reconciliation.Repository, row.Repository) {
			repositoryNode = reconciliation.NodeID
		}
		return nil
	})
	if err != nil {
		return s.finishFailure(ctx, organization, row, err)
	}
	if !live {
		return s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			writer, ok := w.(projectstore.GitHubPullRequestRefreshWriter)
			if !ok {
				return s.deps.Unavailable
			}
			err := writer.CancelGitHubPullRequestRefresh(ctx, row)
			if errors.Is(err, corestore.ErrRevisionConflict) {
				return nil
			}
			return err
		})
	}
	if !current {
		return s.finishFailure(ctx, organization, row, githubcontract.Permanent(corestore.ErrRevisionConflict))
	}
	if row.ObservedRequestCount == 0 {
		var first tracker.NativeWorkItemID
		if err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			links, err := r.Links(ctx, row.ChangeID, "", 1)
			if err == nil && len(links) == 1 {
				first = links[0].WorkItemID
			}
			return err
		}); err != nil {
			return s.finishFailure(ctx, organization, row, err)
		}
		if first != "" && first != row.WorkItemID {
			row.WorkItemID = first
			return s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
				writer, ok := w.(projectstore.GitHubPullRequestRefreshWriter)
				if !ok {
					return s.deps.Unavailable
				}
				return writer.RequestGitHubPullRequests(ctx, []projectstore.GitHubPullRequestRefresh{row})
			})
		}
	}
	if row.ObservedRequestCount == row.RequestCount && row.Observation != nil {
		return s.completeObservation(ctx, organization, row, *row.Observation)
	}
	owner, name, ok := githubwebhook.SplitRepositoryFullName(row.Repository)
	if !ok {
		return s.finishFailure(ctx, organization, row, githubcontract.Permanent(errors.New("pull request repository identity is invalid")))
	}
	snapshot, err := s.deps.Backend.Reconcile(ctx, githubcontract.ReconcileRequest{Profile: "native", Repository: githubcontract.RepositoryTarget{Owner: owner, Name: name, NodeID: repositoryNode}, TargetedPullRequests: true, Through: s.deps.Now(), Hydrations: []githubcontract.HydrationRequest{{ObjectKind: "pull_request", ObjectKey: strconv.Itoa(row.Number), GitHubNumber: row.Number, HeadSHA: row.RequestedHeadSHA, RequestCount: int(row.RequestCount)}}})
	if err != nil {
		return s.finishFailure(ctx, organization, row, err)
	}
	if snapshot.Repository.Owner != "" && snapshot.Repository.Name != "" && !strings.EqualFold(snapshot.Repository.Owner+"/"+snapshot.Repository.Name, row.Repository) || repositoryNode != "" && snapshot.Repository.NodeID != "" && snapshot.Repository.NodeID != repositoryNode {
		return s.finishFailure(ctx, organization, row, githubcontract.Permanent(errors.New("pull request refresh source identity changed")))
	}
	if snapshot.Repository.NodeID == "" || snapshot.Repository.Owner == "" || snapshot.Repository.Name == "" || len(snapshot.PullRequests) != 1 || len(snapshot.PullRequestDetails) != 1 {
		return s.finishFailure(ctx, organization, row, errors.New("pull request refresh source is incomplete"))
	}
	source, details := snapshot.PullRequests[0], snapshot.PullRequestDetails[0]
	if source.Number > 0 && source.Number != row.Number || details.Number > 0 && details.Number != row.Number || source.HeadSHA != "" && details.HeadSHA != "" && source.HeadSHA != details.HeadSHA {
		return s.finishFailure(ctx, organization, row, githubcontract.Permanent(errors.New("pull request refresh identity changed")))
	}
	if source.Number != row.Number || details.Number != row.Number || source.HeadSHA == "" || details.HeadSHA == "" || source.UpdatedAt.IsZero() || details.Checks == nil || details.Reviews == nil {
		return s.finishFailure(ctx, organization, row, errors.New("pull request refresh details are incomplete"))
	}
	state := strings.ToLower(strings.TrimSpace(source.State))
	if state != "open" && state != "closed" {
		return s.finishFailure(ctx, organization, row, errors.New("pull request refresh state is invalid"))
	}
	if !source.MergedAt.IsZero() {
		state = "merged"
	}
	var mergeable *bool
	if details.MergeableState != nil {
		value := pullrequests.MergeableFromState(*details.MergeableState)
		if value.Known {
			mergeable = new(value.Value)
		}
	}
	observation := projectstore.GitHubPullRequestObservation{Repository: row.Repository, Number: row.Number, HeadSHA: source.HeadSHA, SourceUpdatedAt: source.UpdatedAt, ObservedAt: s.deps.Now().UTC(), State: state, Draft: source.Draft, Mergeable: mergeable, Checks: *details.Checks, Reviews: *details.Reviews}
	if snapshot.Repository.DefaultBranch != "" {
		observation.RepositoryDefaultBranch = new(snapshot.Repository.DefaultBranch)
	}
	if source.BaseRef != "" {
		observation.MergeSHA, observation.BaseRef, observation.MergedBy = new(source.MergeSHA), new(source.BaseRef), new(source.MergedBy)
	}
	if !source.MergedAt.IsZero() {
		if source.BaseRef == "" || !validCommitID(source.MergeSHA) {
			return s.finishFailure(ctx, organization, row, errors.New("pull request merge source is incomplete"))
		}
		observation.MergedAt = new(source.MergedAt)
	}
	return s.completeObservation(ctx, organization, row, observation)
}

func (s *Service) completeObservation(ctx context.Context, organization tracker.OrganizationID, row projectstore.GitHubPullRequestRefresh, observation projectstore.GitHubPullRequestObservation) error {
	err := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(projectstore.GitHubPullRequestRefreshWriter)
		if !ok {
			return s.deps.Unavailable
		}
		live, current, err := s.refreshOrigin(ctx, w, row)
		if err != nil {
			return err
		}
		if !live {
			return writer.CancelGitHubPullRequestRefresh(ctx, row)
		}
		if !current {
			return writer.SupersedeGitHubPullRequestRefresh(ctx, row)
		}
		return writer.CompleteGitHubPullRequestRefresh(ctx, row, observation)
	})
	if err != nil {
		return s.finishFailure(ctx, organization, row, err)
	}
	return nil
}

func validCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
