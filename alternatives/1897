package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresLinkedAndCutoffReadsRecheckRole(t *testing.T) {
	for _, kind := range []string{"linked", "export_cutoff"} {
		t.Run(kind, func(t *testing.T) {
			repository, f, owner, expected, changes := newPostgresExportLinkedFixture(t)
			var item tracker.NativeWorkItemID
			for id, name := range expected {
				if name == "second" {
					item = id
				}
			}
			key, path := "", f.base+"/work-items/"+string(item)+"/pull-requests"
			if kind == "export_cutoff" {
				seedExportComment(t, repository, f.service, f.project, item, 9000, strings.Repeat("x", operatortool.WorkBulkExportMaxBytes))
				path = f.base + "/work-items/export"
			}
			if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
				if kind == "linked" {
					row, err := r.Change(t.Context(), changes[item])
					key = row.PayloadObjectKey
					return err
				}
				row, err := r.CollaborationVersion(t.Context(), fmt.Sprintf("comment_%s_%d", item, 10000), 1)
				key = row.PayloadObjectKey
				return err
			}); err != nil {
				t.Fatal(err)
			}
			changed := false
			original := f.service.config.RunDetails
			f.service.config.RunDetails = &changeReadInterceptionStore{DetailStore: original, key: key, afterRead: func(ctx context.Context) error {
				return repository.WriteTenant(ctx, "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(ctx, owner.identity.Subject)
					if err != nil {
						return err
					}
					member.Role = "reporter"
					changed = true
					return w.UpdateAccessMember(ctx, member)
				})
			}}
			defer func() { f.service.config.RunDetails = original }()
			response := f.request(t, owner, http.MethodGet, path, nil)
			if !changed || response.Code == http.StatusOK {
				t.Fatalf("stale role accepted: hook=%v status=%d", changed, response.Code)
			}
		})
	}
}

func TestPostgresLinkedCacheKeepsReporterVisibility(t *testing.T) {
	repository, f, owner, expected, changes := newPostgresExportLinkedFixture(t)
	var primary, secondary tracker.NativeWorkItemID
	for id, name := range expected {
		if name == "first" {
			primary = id
		} else {
			secondary = id
		}
	}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: "org_security", ProjectID: f.project, WorkItemID: secondary, ChangeID: changes[primary], CreatedAt: f.service.config.now()})
	}); err != nil {
		t.Fatal(err)
	}
	path := f.base + "/work-items/" + string(secondary) + "/pull-requests"
	response := f.request(t, owner, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var cached []pullRequestView
	decodeHubResponse(t, response, &cached)
	if len(cached) != 2 {
		t.Fatalf("owner cache items=%d", len(cached))
	}
	response = f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(primary), nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	issue.Revision++
	issue.Actor.PrincipalID = "other_principal"
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(f.service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_security", ProjectID: f.project, WorkItemID: primary, RecordID: string(primary), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), f.project, primary)
		if err != nil {
			return err
		}
		row.ActorPrincipalID = &issue.Actor.PrincipalID
		if _, err = w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		if _, err = w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		member.Role = "reporter"
		return w.UpdateAccessMember(t.Context(), member)
	}); err != nil {
		t.Fatal(err)
	}
	response = f.request(t, owner, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var visible []pullRequestView
	decodeHubResponse(t, response, &visible)
	if len(visible) != 1 || visible[0].ChangeID != changes[secondary] {
		t.Fatalf("reporter received cached private Change: %+v", visible)
	}
}
