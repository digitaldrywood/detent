package workitemchange_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeReadInterceptionStore struct {
	attachment.DetailStore
	mu        sync.Mutex
	key       string
	afterRead func(context.Context) error
}

func (s *changeReadInterceptionStore) intercept(key string, afterRead func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key, s.afterRead = key, afterRead
}

func (s *changeReadInterceptionStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	raw, err := s.DetailStore.ReadDetail(ctx, organization, ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var callback func(context.Context) error
	if s.key == ref.Key {
		callback, s.afterRead = s.afterRead, nil
	}
	s.mu.Unlock()
	if callback != nil {
		if err := callback(ctx); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func TestPostgresBoundedChangeReadRechecksRoleAfterHydration(t *testing.T) {
	t.Parallel()
	details := &changeReadInterceptionStore{DetailStore: hubtest.NewDetails()}
	h := hubfixture.NewHostedSecurity(t, func(cfg *hubfixture.Config) {
		cfg.RunDetails = details
		cfg.WebhookMaintenanceInterval = time.Hour
	})
	repository := h.Repository
	owner := h.User(t, "review_admin", "owner", "review-admin@example.test", "write", "")
	items := h.Base + "/work-items"
	created := h.Request(t, owner, http.MethodPost, items, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "bounded-role-issue"}, Title: "Example authorization source " + testenv.DistinctText("bounded authority primary title", 8), Body: testenv.DistinctText("bounded authority primary body", 32) + "\n" + hubfixture.IssueContractSections, State: "Todo"})
	hubfixture.RequireStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, created, &issue)
	created = h.Request(t, owner, http.MethodPost, items, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "bounded-secondary-issue"}, Title: "Example transferred issue " + testenv.DistinctText("bounded authority secondary title", 8), Body: testenv.DistinctText("bounded authority secondary body", 32) + "\n" + hubfixture.IssueContractSections, State: "Todo"})
	hubfixture.RequireStatus(t, created, http.StatusOK)
	var secondary tracker.NativeIssue
	hubfixture.Decode(t, created, &secondary)
	changes := items + "/" + string(issue.WorkItemID) + "/changes"
	created = h.Request(t, owner, http.MethodPost, changes, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "bounded-role-change"}, Title: "Example private Change", LinkedIssues: []tracker.NativeWorkItemID{secondary.WorkItemID}})
	hubfixture.RequireStatus(t, created, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, created, &change)
	var row eventstore.Change
	if err := repository.ReadTenant(t.Context(), h.Primary, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.Change(t.Context(), change.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	key := hubtest.Runner(t, repository, h.Primary, hubtest.RunnerOptions{
		Projects:   []tracker.ProjectID{h.Project},
		Operations: []string{runnerauth.Read},
		Now:        h.Now(),
	}).Credential
	var keyRevoked bool
	details.intercept(row.PayloadObjectKey, func(ctx context.Context) error {
		keyRevoked = true
		return repository.WriteTenant(ctx, h.Primary, corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(ctx, key.ID, time.Now().UTC()) })
	})
	keyRead := h.Hub.Request(t, http.MethodGet, changes+"/"+change.ID+"?view=bounded", key.Token, nil)
	if !keyRevoked {
		t.Fatal("key was not revoked during source hydration")
	}
	if keyRead.Code == http.StatusOK || strings.Contains(keyRead.Body.String(), "Example private Change") {
		t.Fatalf("revoked key received Change data: %d %s", keyRead.Code, keyRead.Body.String())
	}
	var changed bool
	details.intercept(row.PayloadObjectKey, func(ctx context.Context) error {
		changed = true
		return repository.WriteTenant(ctx, h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(ctx, owner.Identity.Subject)
			if err != nil {
				return err
			}
			member.Role = "reporter"
			return w.UpdateAccessMember(ctx, member)
		})
	})
	response := h.Request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	if !changed {
		t.Fatal("role did not change during source hydration")
	}
	if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "Example private Change") {
		t.Fatalf("downgraded member received Change data: %d %s", response.Code, response.Body.String())
	}
	response = h.Request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var oldVersion eventstore.CollaborationVersion
	if err := repository.ReadTenant(t.Context(), h.Primary, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		oldVersion, err = r.CollaborationVersion(t.Context(), string(secondary.WorkItemID), int64(secondary.Revision))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	transferred := secondary
	transferred.Revision++
	transferred.Actor.PrincipalID = "other_principal"
	raw, err := json.Marshal(transferred)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: h.Primary, ProjectID: h.Project, WorkItemID: secondary.WorkItemID, RecordID: string(secondary.WorkItemID), Revision: int64(transferred.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	var transferredDuringRead bool
	details.intercept(oldVersion.PayloadObjectKey, func(ctx context.Context) error {
		transferredDuringRead = true
		return repository.WriteTenant(ctx, h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, h.Project); err != nil {
				return err
			}
			row, err := w.WorkItem(ctx, h.Project, secondary.WorkItemID)
			if err != nil {
				return err
			}
			row.ActorPrincipalID = &transferred.Actor.PrincipalID
			if _, err := w.UpdateWorkItem(ctx, row); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(ctx, newVersion)
			return err
		})
	})
	response = h.Request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	if !transferredDuringRead {
		t.Fatal("secondary ownership did not change during hydration")
	}
	if response.Code == http.StatusOK || strings.Contains(response.Body.String(), string(secondary.WorkItemID)) {
		t.Fatalf("reporter received a no-longer-visible linked issue: %d %s", response.Code, response.Body.String())
	}
	response = h.Request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	if strings.Contains(response.Body.String(), string(secondary.WorkItemID)) {
		t.Fatal("reporter received an inaccessible linked issue on reread")
	}
	foreignPath := items + "/" + string(secondary.WorkItemID) + "/changes/" + change.ID + "?view=bounded"
	foreignRead := h.Request(t, owner, http.MethodGet, foreignPath, nil)
	if foreignRead.Code != http.StatusNotFound || strings.Contains(foreignRead.Body.String(), "Example private Change") {
		t.Fatalf("inaccessible linked issue exposed the Change: %d %s", foreignRead.Code, foreignRead.Body.String())
	}
}
