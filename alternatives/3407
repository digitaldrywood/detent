package pgstoretest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HistoryObjects struct {
	Mu      sync.Mutex
	Objects map[string][]byte
	encoder attachment.ArchiveEncoder
}

func NewHistoryObjects() *HistoryObjects { return &HistoryObjects{Objects: make(map[string][]byte)} }

func (s *HistoryObjects) StoreDetail(_ context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := s.encoder.Prepare(org, 1, bytes.NewReader(raw))
	if err != nil {
		return ref, err
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Objects[ref.Key] = bytes.Clone(compressed)
	return ref, nil
}

func (s *HistoryObjects) ReadDetail(_ context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(org) != nil {
		return nil, artifact.ErrIntegrity
	}
	s.Mu.Lock()
	compressed, ok := s.Objects[ref.Key]
	compressed = bytes.Clone(compressed)
	s.Mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(r)
	return raw, errors.Join(err, r.Close())
}

func HistoryJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func HistoryActor() tracker.Actor {
	return tracker.Actor{Kind: "human", PrincipalID: "example", DisplayName: "Example actor", ViaKey: "example-key"}
}

func HistoryAt() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC) }

func HistoryEvent(t *testing.T, c *eventstore.Content, org tracker.OrganizationID, id string) eventstore.Event {
	t.Helper()
	value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", Type: "issue.edited", SchemaVersion: 1, Actor: HistoryActor(), RecordedAt: HistoryAt(), Data: tracker.CollaborationData{Revision: 2, Fields: []string{"title"}, Reason: "Example reason"}}
	row, err := c.PrepareEvent(t.Context(), org, HistoryJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

type HistoryFixture struct {
	Content  *eventstore.Content
	Objects  *HistoryObjects
	Event    eventstore.Event
	Snapshot eventstore.CollaborationVersion
	Change   eventstore.Change
	Version  eventstore.Version
	Evidence eventstore.Evidence
	Link     eventstore.Link
	Receipt  eventstore.Receipt
}

func SeedHistory(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, suffix string) HistoryFixture {
	t.Helper()
	f := HistoryFixture{Objects: NewHistoryObjects()}
	f.Content = eventstore.NewContent(f.Objects)
	f.Event = HistoryEvent(t, f.Content, org, "event_"+suffix)
	f.Event.Sequence = 0
	change := tracker.ChangeRequest{ID: "change_" + suffix, OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", LinkedIssues: []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"}, Title: "Example change", Body: "Change body", Revision: 1, CreatedAt: HistoryAt(), UpdatedAt: HistoryAt()}
	var err error
	f.Change, err = f.Content.PrepareChange(t.Context(), org, HistoryJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	version := tracker.ChangeVersion{ID: "version_" + suffix, ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", BaseSHA: "base", MergeBaseSHA: "merge-base", Repository: "owner/repository", PolicyID: "policy", AttemptID: "attempt", External: &tracker.ChangeExternalReference{Provider: "example", ID: "external", URL: "https://example.com/change"}}, Actor: HistoryActor(), CreatedAt: HistoryAt()}
	version.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: "base", HeadSHA: "head", BundleSHA256: artifact.Digest(bytes.Repeat([]byte(string(org)+suffix), 123)[:123]), DiffSHA256: artifact.Digest([]byte("diff")), Bytes: 123}
	f.Version, err = f.Content.PrepareVersion(t.Context(), org, HistoryJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	source, err := f.Content.PrepareSourceBundle(t.Context(), org, bytes.Repeat([]byte(string(org)+suffix), 123)[:123])
	if err != nil {
		t.Fatal(err)
	}
	f.Version.SourceObjectKey, f.Version.SourceSHA256 = source.Key, source.SHA256
	f.Version.SourceBytes, f.Version.SourceContentBytes = source.CompressedSize, source.UncompressedSize
	discussion := tracker.ChangeDiscussion{ID: "discussion_" + suffix, VersionID: version.ID, Body: "Evidence body", Actor: HistoryActor(), CreatedAt: HistoryAt(), Provenance: &tracker.Provenance{Provider: "example", ExternalID: "source_" + suffix, AuthorID: "author"}}
	f.Evidence, err = f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: change.ID, Kind: "discussion"}, HistoryJSON(t, discussion))
	if err != nil {
		t.Fatal(err)
	}
	f.Snapshot, err = f.Content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: change.ProjectID, WorkItemID: change.WorkItemID, RecordID: "snapshot_" + suffix, Revision: 2}, HistoryJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	f.Receipt, err = f.Content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "example", Operation: "create_change", CommandKey: suffix, RequestHash: artifact.Digest([]byte("request")), CreatedAt: HistoryAt()}, []byte(`{"change_id":"`+change.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	f.Link = eventstore.Link{OrganizationID: org, ProjectID: change.ProjectID, WorkItemID: "wi_first_2", ChangeID: change.ID}
	f.Link.CreatedAt = f.Change.CreatedAt
	if err := r.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
		if f.Event, err = w.AppendEvent(t.Context(), f.Event); err != nil {
			return err
		}
		if f.Snapshot, err = w.CreateCollaborationVersion(t.Context(), f.Snapshot); err != nil {
			return err
		}
		if f.Change, err = w.CreateChange(t.Context(), f.Change); err != nil {
			return err
		}
		if f.Version, err = w.CreateVersion(t.Context(), f.Version); err != nil {
			return err
		}
		if f.Evidence, err = w.AppendEvidence(t.Context(), f.Evidence); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), f.Link); err != nil {
			return err
		}
		f.Receipt, err = w.PutReceipt(t.Context(), f.Receipt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.Change.EvidenceSequence = 1
	return f
}

type ReportObjectReadProbe struct {
	attachment.DetailStore
	Check func()
}

func (p ReportObjectReadProbe) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	p.Check()
	return p.DetailStore.ReadDetail(ctx, org, ref)
}
