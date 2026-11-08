package hubserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) maintainGitHubReconciliationRecords(ctx context.Context) error {
	if s.config.ReconcileBackend == nil || s.config.ImportBackend == nil {
		return nil
	}
	if s.config.TenantRepository == nil || s.database == nil {
		return ErrPostgresFeatureUnavailable
	}
	organization := s.database.hostedOrganization
	var row projectstore.GitHubReconciliation
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubReconciliationReader, ok := r.(projectstore.GitHubReconciliationReader)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		row, err = gitHubReconciliationReader.NextGitHubReconciliation(ctx, s.config.now())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	scope, err := s.gitHubIntegrationScope(ctx, organization, row.ProjectID)
	if err == nil && !strings.EqualFold(scope.integrationRepository, row.Repository) {
		err = errors.New("GitHub reconciliation repository changed")
	}
	owner, name, _ := splitRepositoryFullName(row.Repository)
	var snapshot ReconcileSnapshot
	if err == nil {
		snapshot, err = s.config.ReconcileBackend.Reconcile(ctx, ReconcileRequest{Profile: "native", Repository: RepositoryTarget{Owner: owner, Name: name, NodeID: row.NodeID}, SkipIssues: true, SkipRepository: true, Through: s.config.now()})
	}
	if err == nil && (snapshot.Repository.NodeID == "" || row.NodeID != "" && snapshot.Repository.NodeID != row.NodeID || !strings.EqualFold(snapshot.Repository.Owner+"/"+snapshot.Repository.Name, row.Repository)) {
		err = errors.New("GitHub reconciliation source identity changed")
	}
	if err == nil {
		row.NodeID = snapshot.Repository.NodeID
	}
	var linked eventstore.LinkedSource
	if err == nil {
		err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			var err error
			gitHubReconciliationReader, ok := r.(projectstore.GitHubReconciliationReader)
			if !ok {
				return ErrPostgresFeatureUnavailable
			}
			linked, err = gitHubReconciliationReader.NextGitHubLinkedSource(ctx, row.ProjectID, row.AfterWorkItem)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			row.AfterWorkItem = ""
			err = nil
		} else if err == nil {
			_, repository, number, parseErr := tracker.ParseGitHubIssueURL(linked.URL)
			err = parseErr
			if err == nil && !strings.EqualFold(repository, row.Repository) {
				err = errors.New("GitHub linked source repository changed")
			}
			if err == nil {
				page, fetchErr := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: repository, IssueNumber: number, Stage: "issue"})
				err = fetchErr
				if err == nil && (page.Issue == nil || page.Issue.Number != number) {
					err = errors.New("GitHub reconciliation source is incomplete")
				}
				if err == nil {
					source := linkedSnapshotFromSource(*page.Issue, s.config.now())
					key, hashErr := nativeCommandHash(source)
					err = hashErr
					if err == nil {
						_, err = s.applyLinkedIssueRecord(ctx, scope, linked.WorkItemID, "github.reconcile", tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: key}, Snapshot: source})
					}
				}
			}
			if err == nil {
				row.AfterWorkItem = linked.WorkItemID
			}
		}
	}
	row.NextAttemptAt = s.config.now().Add(time.Second)
	if err != nil {
		row.Attempts++
		row.NextAttemptAt = s.config.now().Add(time.Duration(float64(time.Second*time.Duration(1<<min(row.Attempts, 8))) * (0.5 + s.config.jitter())))
	} else {
		row.Attempts = 0
		if row.AfterWorkItem == "" {
			row.NextAttemptAt = s.config.now().Add(time.Duration(float64(s.config.ReconcileInterval) * (0.5 + s.config.jitter())))
		}
	}
	writeErr := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		gitHubReconciliationWriter, ok := w.(projectstore.GitHubReconciliationWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return gitHubReconciliationWriter.SaveGitHubReconciliation(ctx, row)
	})
	return errors.Join(err, writeErr)
}
