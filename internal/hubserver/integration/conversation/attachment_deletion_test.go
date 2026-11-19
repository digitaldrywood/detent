package conversation_test

import (
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAttachmentDeletionReleasesQuotaOnce(t *testing.T) {
	t.Parallel()
	hub := hubfixture.OpenConversationHostedEntry(t)
	const size = int64(100)
	assignAttachmentQuota(t, hub, size)
	owner := hub.User(t, "attachment-owner", "owner", "owner@example.test", "write", "")
	base := hub.Base + "/attachment-metadata"
	metadata := func(name string) attachment.Metadata {
		return attachment.Metadata{ID: conversation.NewAttachmentID(), Name: name, ContentType: "image/png", Size: size, SHA256: strings.Repeat("a", 64), Width: 1, Height: 1}
	}
	original, replacement, third := metadata("original.png"), metadata("replacement.png"), metadata("third.png")
	foreign := tracker.OrganizationID("org_attachment_foreign")
	hubfixture.SeedOrganization(t, hub.Repository, foreign, "example-foreign-admin", hub.Now())
	foreignProject := hubtest.Project(t, hub.Repository, foreign, hubtest.ProjectOptions{})
	foreignAttachment := metadata("foreign.png")
	foreignAttachment.ProjectID = string(foreignProject.ID)
	foreignAttachment.Uploader = hubfixture.AdminCredentialID
	foreignAttachment.CreatedAt = hub.Now().UTC()
	if err := hub.Repository.WriteTenant(t.Context(), foreign, corestore.Serving, func(writer tenantstore.Writer) error {
		return writer.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: foreign, Metadata: foreignAttachment, Kind: "attachment"})
	}); err != nil {
		t.Fatal(err)
	}

	upload := func(metadata attachment.Metadata, want int) {
		t.Helper()
		hubfixture.RequireStatus(t, hub.BrowserRequest(t, owner, "owner", http.MethodPost, base, metadata), want)
	}
	finish := func(audience tracker.OrganizationID, attachmentID string, want int) {
		t.Helper()
		hubfixture.RequireStatus(t, hub.AttachmentDeletionRequest(t, audience, attachmentID), want)
	}
	upload(original, http.StatusCreated)
	upload(replacement, http.StatusTooManyRequests)
	hubfixture.RequireStatus(t, hub.BrowserRequest(t, owner, "owner", http.MethodDelete, base+"/"+foreignAttachment.ID, nil), http.StatusNotFound)
	finish(hub.Primary, foreignAttachment.ID, http.StatusNotFound)
	finish(hub.Primary, original.ID, http.StatusNotFound)
	hubfixture.RequireStatus(t, hub.BrowserRequest(t, owner, "owner", http.MethodDelete, base+"/"+original.ID, nil), http.StatusNoContent)
	hubfixture.RequireStatus(t, hub.BrowserRequest(t, owner, "owner", http.MethodDelete, base+"/"+original.ID, nil), http.StatusNoContent)
	hubfixture.RequireStatus(t, hub.BrowserRequest(t, owner, "owner", http.MethodGet, base+"/"+original.ID, nil), http.StatusNotFound)
	hubfixture.RequireStatus(t, hub.BrowserRequest(t, owner, "owner", http.MethodPost, "/internal/v1/attachments/deleted", map[string]string{"id": original.ID}), http.StatusNotFound)
	finish(foreign, original.ID, http.StatusUnauthorized)
	upload(replacement, http.StatusTooManyRequests)

	finish(hub.Primary, original.ID, http.StatusNoContent)
	upload(original, http.StatusCreated)
	upload(replacement, http.StatusCreated)
	finish(hub.Primary, original.ID, http.StatusNoContent)
	upload(third, http.StatusTooManyRequests)
	if err := hub.Repository.ReadTenant(t.Context(), foreign, corestore.Serving, func(reader tenantstore.Reader) error {
		stored, err := reader.Attachment(t.Context(), foreignProject.ID, foreignAttachment.ID)
		if err == nil && (stored.Metadata.DeletedAt != nil || stored.ObjectDeletedAt != nil) {
			t.Fatal("the foreign attachment was deleted")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func assignAttachmentQuota(t *testing.T, hub *hubfixture.ConversationHostedEntry, size int64) {
	t.Helper()
	base := hub.Config.Hosted.Plans.Plans[0]
	allowances := maps.Clone(base.Allowances)
	allowances["collaboration_bytes"] = size
	quota := billingstore.PlanReference{ID: "attachment_lifecycle", Version: 1}
	if err := hub.Repository.WriteTenant(t.Context(), hub.Primary, corestore.Serving, func(writer tenantstore.Writer) error {
		ctx := t.Context()
		if err := writer.CreateBillingPlan(ctx, billingstore.Plan{OrganizationID: hub.Primary, PlanReference: quota, Name: "Attachment lifecycle", Features: base.Features, Allowances: allowances}); err != nil {
			return err
		}
		assignment, err := writer.BillingAssignment(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			assignment.OrganizationID = hub.Primary
		} else if err != nil {
			return err
		}
		assignment.Base = quota
		return writer.SetBillingAssignment(ctx, assignment, assignment.Revision)
	}); err != nil {
		t.Fatal(err)
	}
}
