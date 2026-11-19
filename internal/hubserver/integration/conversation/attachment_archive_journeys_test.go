package conversation_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentArchiveQuotaAndRetention(t *testing.T) {
	t.Parallel()
	t.Run("exact storage limit", func(t *testing.T) {
		fixture := openAttachmentArchiveJourney(t)
		first, second, replacement := fixture.issue(t, "First"), fixture.issue(t, "Second"), fixture.issue(t, "Replacement")
		ref := fixture.reference(t, "first archive data")
		fixture.allowAdditionalBytes(t, ref.CompressedSize)
		for _, item := range []string{first, second} {
			hubfixture.RequireStatus(t, fixture.reserve(t, item, ref), http.StatusOK)
			fixture.publish(t, item, ref, http.StatusCreated)
			fixture.read(t, item, ref, http.StatusOK)
		}
		extra := fixture.reference(t, "additional archive data")
		hubfixture.RequireStatus(t, fixture.reserve(t, first, extra), http.StatusTooManyRequests)
		changed := ref
		changed.SchemaVersion++
		fixture.publish(t, first, changed, http.StatusConflict)

		fixture.deleteIssue(t, first)
		fixture.deleteIssue(t, second)
		fixture.requireRetention(t, ref, first, second)
		fixture.publish(t, replacement, ref, http.StatusCreated)
		fixture.read(t, replacement, ref, http.StatusOK)
	})
	t.Run("storage exhausted", func(t *testing.T) {
		fixture := openAttachmentArchiveJourney(t)
		item := fixture.issue(t, "No capacity")
		ref := fixture.reference(t, "archive data under exhausted quota")
		fixture.allowAdditionalBytes(t, ref.CompressedSize-1)
		hubfixture.RequireStatus(t, fixture.reserve(t, item, ref), http.StatusTooManyRequests)
		fixture.publish(t, item, ref, http.StatusNotFound)
		fixture.read(t, item, ref, http.StatusNotFound)
	})
}

func TestAttachmentArchiveConcurrentReservations(t *testing.T) {
	t.Parallel()
	t.Run("distinct blobs compete for one allocation", func(t *testing.T) {
		fixture := openAttachmentArchiveJourney(t)
		items := [2]string{fixture.issue(t, "First"), fixture.issue(t, "Second")}
		refs := [2]attachment.ArchiveReference{fixture.reference(t, "first archive bytes"), fixture.reference(t, "second archive bytes")}
		fixture.allowAdditionalBytes(t, max(refs[0].CompressedSize, refs[1].CompressedSize))

		responses := fixture.reserveTogether(t, items, refs)
		winner, loser := 0, 1
		if responses[1].Code == http.StatusOK {
			winner, loser = 1, 0
		}
		hubfixture.RequireStatus(t, responses[winner], http.StatusOK)
		hubfixture.RequireStatus(t, responses[loser], http.StatusTooManyRequests)
		for index, ref := range refs {
			fixture.read(t, items[index], ref, http.StatusNotFound)
		}
		fixture.publish(t, items[winner], refs[winner], http.StatusCreated)
		fixture.publish(t, items[loser], refs[loser], http.StatusNotFound)
		fixture.read(t, items[winner], refs[winner], http.StatusOK)
	})
	t.Run("shared blob survives failed publication", func(t *testing.T) {
		fixture := openAttachmentArchiveJourney(t)
		items := [2]string{fixture.issue(t, "Published"), fixture.issue(t, "Deleted")}
		ref := fixture.reference(t, "shared archive bytes")
		fixture.allowAdditionalBytes(t, ref.CompressedSize)

		responses := fixture.reserveTogether(t, items, [2]attachment.ArchiveReference{ref, ref})
		for index, response := range responses {
			hubfixture.RequireStatus(t, response, http.StatusOK)
			fixture.read(t, items[index], ref, http.StatusNotFound)
		}
		fixture.publish(t, items[0], ref, http.StatusCreated)
		fixture.deleteIssue(t, items[1])
		fixture.publish(t, items[1], ref, http.StatusNotFound)
		fixture.read(t, items[0], ref, http.StatusOK)
		extra := fixture.reference(t, "other archive data")
		hubfixture.RequireStatus(t, fixture.reserve(t, items[0], extra), http.StatusTooManyRequests)
	})
}

type attachmentArchiveJourney struct {
	hub   *hubfixture.ConversationHostedEntry
	owner hubfixture.HostedUser
}

func openAttachmentArchiveJourney(t *testing.T) attachmentArchiveJourney {
	t.Helper()
	hub := hubfixture.OpenConversationHostedEntry(t)
	return attachmentArchiveJourney{hub: hub, owner: hub.User(t, "archive-owner", "owner", "owner@example.test", "write", "")}
}

func (f attachmentArchiveJourney) issue(t *testing.T, title string) string {
	t.Helper()
	response := f.hub.BrowserRequest(t, f.owner, "owner", http.MethodPost, f.hub.Base+"/work-items", tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("issue")},
		Title:    title + " archive source " + testenv.DistinctText(title, 6),
		Body:     testenv.DistinctText(title, 24) + "\n\n" + hubfixture.IssueBody(),
		State:    "Todo",
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return string(issue.WorkItemID)
}

func (f attachmentArchiveJourney) reference(t *testing.T, content string) attachment.ArchiveReference {
	t.Helper()
	ref, _, err := attachment.PrepareArchive(string(f.hub.Primary), 1, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func (f attachmentArchiveJourney) allowAdditionalBytes(t *testing.T, bytes int64) {
	t.Helper()
	var used int64
	if err := f.hub.Repository.ReadTenant(t.Context(), f.hub.Primary, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		used, err = reader.CollaborationBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assignAttachmentQuota(t, f.hub, used+bytes)
}

func (f attachmentArchiveJourney) request(t *testing.T, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	return f.hub.BrowserRequest(t, f.owner, "owner", method, f.hub.Base+"/attachment-metadata/archives/"+path, payload)
}

func (f attachmentArchiveJourney) reserve(t *testing.T, item string, ref attachment.ArchiveReference) *httptest.ResponseRecorder {
	t.Helper()
	return f.request(t, http.MethodPost, item+"/check", map[string]attachment.ArchiveReference{"reference": ref})
}

func (f attachmentArchiveJourney) reserveTogether(t *testing.T, items [2]string, refs [2]attachment.ArchiveReference) [2]*httptest.ResponseRecorder {
	t.Helper()
	start := make(chan struct{})
	var done sync.WaitGroup
	var responses [2]*httptest.ResponseRecorder
	for index, ref := range refs {
		done.Go(func() {
			<-start
			responses[index] = f.reserve(t, items[index], ref)
		})
	}
	close(start)
	done.Wait()
	for index, response := range responses {
		if response == nil {
			t.Fatalf("reservation request %q did not complete", items[index])
		}
	}
	return responses
}

func (f attachmentArchiveJourney) publish(t *testing.T, item string, ref attachment.ArchiveReference, status int) {
	t.Helper()
	hubfixture.RequireStatus(t, f.request(t, http.MethodPost, item, ref), status)
}

func (f attachmentArchiveJourney) read(t *testing.T, item string, ref attachment.ArchiveReference, status int) {
	t.Helper()
	response := f.request(t, http.MethodGet, item+"/"+ref.SHA256, nil)
	hubfixture.RequireStatus(t, response, status)
	if status == http.StatusOK {
		var stored attachment.ArchiveReference
		hubfixture.Decode(t, response, &stored)
		if stored.Key != ref.Key || stored.SHA256 != ref.SHA256 {
			t.Fatalf("source %s returned the wrong archive: %+v", item, stored)
		}
	}
}

func (f attachmentArchiveJourney) deleteIssue(t *testing.T, item string) {
	t.Helper()
	if err := f.hub.Repository.WriteTenant(t.Context(), f.hub.Primary, corestore.Serving, func(writer tenantstore.Writer) error {
		return writer.DeleteWorkItem(t.Context(), f.hub.Project, tracker.NativeWorkItemID(item))
	}); err != nil {
		t.Fatal(err)
	}
}

func (f attachmentArchiveJourney) requireRetention(t *testing.T, ref attachment.ArchiveReference, deletedItems ...string) {
	t.Helper()
	ctx := t.Context()
	cutoff := f.hub.Now().Add(2 * attachment.OrphanTTL)
	orphan := attachment.Metadata{
		ID: conversation.NewAttachmentID(), ProjectID: string(f.hub.Project), Uploader: hubfixture.AdminCredentialID,
		Name: "orphan.png", ContentType: "image/png", SHA256: strings.Repeat("a", 64), Size: 84, Width: 1, Height: 1,
		CreatedAt: f.hub.Now(),
	}
	if err := f.hub.Repository.WriteTenant(ctx, f.hub.Primary, corestore.Serving, func(writer tenantstore.Writer) error {
		return writer.CreateAttachment(ctx, attachmentstore.Record{OrganizationID: f.hub.Primary, Metadata: orphan, Kind: "attachment"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Repository.ReadTenant(ctx, f.hub.Primary, corestore.Serving, func(reader tenantstore.Reader) error {
		for _, item := range deletedItems {
			_, err := reader.AttachmentArchive(ctx, attachmentstore.ArchiveQuery{ProjectID: f.hub.Project, WorkItemID: tracker.NativeWorkItemID(item), Digest: ref.SHA256})
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("deleted source %s still has a reference, error %v", item, err)
			}
		}
		candidates, err := reader.(attachmentstore.ExpiryReader).AttachmentDeletionCandidates(ctx, cutoff, 100)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(candidates, func(row attachmentstore.Record) bool { return row.Metadata.ID == orphan.ID }) {
			t.Fatal("the old orphan is missing from attachment expiry candidates")
		}
		if slices.ContainsFunc(candidates, func(row attachmentstore.Record) bool { return row.ObjectKey == ref.Key }) {
			t.Fatal("attachment expiry would delete the published archive")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
