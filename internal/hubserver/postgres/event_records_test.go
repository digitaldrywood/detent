package postgres_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type historyObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newHistoryObjects() *historyObjects { return &historyObjects{objects: make(map[string][]byte)} }
func (s *historyObjects) StoreDetail(_ context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(org, 1, bytes.NewReader(raw))
	if err != nil {
		return ref, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[ref.Key] = bytes.Clone(compressed)
	return ref, nil
}
func (s *historyObjects) ReadDetail(_ context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(org) != nil {
		return nil, artifact.ErrIntegrity
	}
	s.mu.Lock()
	compressed, ok := s.objects[ref.Key]
	compressed = bytes.Clone(compressed)
	s.mu.Unlock()
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

func historyJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func historyActor() tracker.Actor {
	return tracker.Actor{Kind: "human", PrincipalID: "example", DisplayName: "Example actor", ViaKey: "example-key"}
}
func historyAt() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC) }
func historyEvent(t *testing.T, c *eventstore.Content, org tracker.OrganizationID, id string) eventstore.Event {
	t.Helper()
	value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", Type: "issue.edited", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt(), Data: tracker.CollaborationData{Revision: 2, Fields: []string{"title"}, Reason: "Example reason"}}
	row, err := c.PrepareEvent(t.Context(), org, historyJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

type historyFixture struct {
	content  *eventstore.Content
	objects  *historyObjects
	event    eventstore.Event
	snapshot eventstore.CollaborationVersion
	change   eventstore.Change
	version  eventstore.Version
	evidence eventstore.Evidence
	link     eventstore.Link
	receipt  eventstore.Receipt
}

func seedHistory(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, suffix string) historyFixture {
	t.Helper()
	f := historyFixture{objects: newHistoryObjects()}
	f.content = eventstore.NewContent(f.objects)
	f.event = historyEvent(t, f.content, org, "event_"+suffix)
	f.event.Sequence = 0
	change := tracker.ChangeRequest{ID: "change_" + suffix, OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", LinkedIssues: []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"}, Title: "Example change", Body: "Change body", Revision: 1, CreatedAt: historyAt(), UpdatedAt: historyAt()}
	var err error
	f.change, err = f.content.PrepareChange(t.Context(), org, historyJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	version := tracker.ChangeVersion{ID: "version_" + suffix, ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", BaseSHA: "base", MergeBaseSHA: "merge-base", Repository: "owner/repository", PolicyID: "policy", AttemptID: "attempt", External: &tracker.ChangeExternalReference{Provider: "example", ID: "external", URL: "https://example.com/change"}}, Actor: historyActor(), CreatedAt: historyAt()}
	f.version, err = f.content.PrepareVersion(t.Context(), org, historyJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	discussion := tracker.ChangeDiscussion{ID: "discussion_" + suffix, VersionID: version.ID, Body: "Evidence body", Actor: historyActor(), CreatedAt: historyAt(), Provenance: &tracker.Provenance{Provider: "example", ExternalID: "source_" + suffix, AuthorID: "author"}}
	f.evidence, err = f.content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: change.ID, Kind: "discussion"}, historyJSON(t, discussion))
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot, err = f.content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: change.ProjectID, WorkItemID: change.WorkItemID, RecordID: "snapshot_" + suffix, Revision: 2}, historyJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	f.receipt, err = f.content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "example", Operation: "create_change", CommandKey: suffix, RequestHash: artifact.Digest([]byte("request")), CreatedAt: historyAt()}, []byte(`{"change_id":"`+change.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	f.link = eventstore.Link{OrganizationID: org, ProjectID: change.ProjectID, WorkItemID: "wi_first_2", ChangeID: change.ID}
	if err := r.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
		if f.event, err = w.AppendEvent(t.Context(), f.event); err != nil {
			return err
		}
		if f.snapshot, err = w.CreateCollaborationVersion(t.Context(), f.snapshot); err != nil {
			return err
		}
		if f.change, err = w.CreateChange(t.Context(), f.change); err != nil {
			return err
		}
		if f.version, err = w.CreateVersion(t.Context(), f.version); err != nil {
			return err
		}
		if f.evidence, err = w.AppendEvidence(t.Context(), f.evidence); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), f.link); err != nil {
			return err
		}
		f.receipt, err = w.PutReceipt(t.Context(), f.receipt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.change.EvidenceSequence = 1
	return f
}

func TestEventChangeTenantRoundTrip(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
		for _, suffix := range []string{"first", "second"} {
			t.Run(string(org)+"/"+suffix, func(t *testing.T) {
				f := seedHistory(t, r, org, suffix)
				if f.event.Sequence < 1 || f.evidence.Sequence != 1 {
					t.Fatalf("missing sequences: event=%d evidence=%d", f.event.Sequence, f.evidence.Sequence)
				}
				var events []eventstore.Event
				var snapshots eventstore.CollaborationVersion
				var change eventstore.Change
				var changes []eventstore.Change
				var versions []eventstore.Version
				var evidence []eventstore.Evidence
				var links []eventstore.Link
				var receipt eventstore.Receipt
				err := r.ReadRecords(t.Context(), org, corestore.Serving, func(reader eventstore.Reader) error {
					var err error
					if events, err = reader.Events(t.Context(), "prj_first", "wi_first_1", f.event.Sequence-1, 1); err != nil {
						return err
					}
					if snapshots, err = reader.CollaborationVersion(t.Context(), f.snapshot.RecordID, 2); err != nil {
						return err
					}
					if change, err = reader.Change(t.Context(), f.change.ID); err != nil {
						return err
					}
					after := ""
					if suffix == "second" {
						after = "change_first"
					}
					if changes, err = reader.Changes(t.Context(), "prj_first", "wi_first_1", after, 1); err != nil {
						return err
					}
					if versions, err = reader.Versions(t.Context(), f.change.ID, 0, 1); err != nil {
						return err
					}
					if evidence, err = reader.Evidence(t.Context(), f.change.ID, 0, 1); err != nil {
						return err
					}
					if receipt, err = reader.Receipt(t.Context(), f.receipt.ActorID, f.receipt.Operation, f.receipt.CommandKey); err != nil {
						return err
					}
					links, err = reader.Links(t.Context(), f.change.ID, "", 1)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, tc := range []struct {
					name      string
					got, want any
				}{
					{"events", events, []eventstore.Event{f.event}},
					{"snapshot", snapshots, f.snapshot},
					{"change", change, f.change},
					{"changes", changes, []eventstore.Change{f.change}},
					{"versions", versions, []eventstore.Version{f.version}},
					{"evidence", evidence, []eventstore.Evidence{f.evidence}},
					{"links", links, []eventstore.Link{f.link}},
					{"receipt", receipt, f.receipt},
				} {
					t.Run(tc.name, func(t *testing.T) {
						if !reflect.DeepEqual(tc.got, tc.want) {
							t.Fatalf("round trip mismatch: got %+v want %+v", tc.got, tc.want)
						}
					})
				}
				response, err := f.content.Receipt(t.Context(), receipt)
				if err != nil || string(response) != `{"change_id":"`+f.change.ID+`"}` {
					t.Fatalf("receipt response=%s error=%v", response, err)
				}
				event, err := f.content.Event(t.Context(), events[0])
				if err != nil || event.Data.Reason != "Example reason" || event.Actor.DisplayName != "Example actor" || event.AggregateSequence != f.event.Sequence {
					t.Fatalf("event payload=%+v error=%v", event, err)
				}
				hydrated, err := f.content.Change(t.Context(), change)
				if err != nil || hydrated.Body != "Change body" || len(hydrated.LinkedIssues) != 2 {
					t.Fatalf("change payload=%+v error=%v", hydrated, err)
				}
				version, err := f.content.Version(t.Context(), versions[0])
				if err != nil || version.External == nil || version.External.ID != "external" || version.AttemptID != "attempt" {
					t.Fatalf("version payload=%+v error=%v", version, err)
				}
				raw, err := f.content.Evidence(t.Context(), evidence[0])
				var discussion tracker.ChangeDiscussion
				if err != nil || json.Unmarshal(raw, &discussion) != nil || discussion.Body != "Evidence body" || discussion.Provenance.AuthorID != "author" {
					t.Fatalf("evidence payload=%s error=%v", raw, err)
				}
				raw, err = f.content.CollaborationVersion(t.Context(), snapshots)
				var snapshot tracker.ChangeRequest
				if err != nil || json.Unmarshal(raw, &snapshot) != nil || snapshot.Body != "Change body" {
					t.Fatalf("snapshot payload=%s error=%v", raw, err)
				}
			})
		}
	}
}

func TestEventChangeReceipts(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedRepository(t, r, "org_first")
	f := seedHistory(t, r, "org_first", "first")
	for _, tc := range []struct {
		name   string
		replay func(eventstore.Writer, bool) error
	}{
		{"event", func(w eventstore.Writer, changed bool) error {
			record := f.event
			record.Sequence = 0
			if changed {
				record.HeadSHA = "other"
			}
			got, err := w.AppendEvent(t.Context(), record)
			if err == nil && !reflect.DeepEqual(got, f.event) {
				t.Error("event receipt changed")
			}
			return err
		}},
		{"snapshot", func(w eventstore.Writer, changed bool) error {
			record := f.snapshot
			if changed {
				record.WorkItemID = "wi_first_2"
			}
			_, err := w.CreateCollaborationVersion(t.Context(), record)
			return err
		}},
		{"change", func(w eventstore.Writer, changed bool) error {
			record := f.change
			if changed {
				record.Title = "other"
			}
			_, err := w.CreateChange(t.Context(), record)
			return err
		}},
		{"version", func(w eventstore.Writer, changed bool) error {
			record := f.version
			if changed {
				record.HeadSHA = "other"
			}
			_, err := w.CreateVersion(t.Context(), record)
			return err
		}},
		{"evidence", func(w eventstore.Writer, changed bool) error {
			record := f.evidence
			record.Sequence = 0
			if changed {
				record.Decision = "other"
			}
			got, err := w.AppendEvidence(t.Context(), record)
			if err == nil && !reflect.DeepEqual(got, f.evidence) {
				t.Error("evidence receipt changed")
			}
			return err
		}},
		{"link", func(w eventstore.Writer, changed bool) error {
			record := f.link
			if changed {
				record.ProjectID = "prj_second"
			}
			return w.LinkIssue(t.Context(), record)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, changed := range []bool{false, true} {
				err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error { return tc.replay(w, changed) })
				if changed && !errors.Is(err, corestore.ErrRevisionConflict) || !changed && err != nil {
					t.Fatalf("changed=%v error=%v", changed, err)
				}
			}
		})
	}
	next := historyEvent(t, f.content, "org_first", "event_next")
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		got, err := w.AppendEvent(t.Context(), next)
		if err == nil && got.Sequence != 2 {
			t.Errorf("replays consumed sequence: %d", got.Sequence)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEventChangeImmutableHistory(t *testing.T) {
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	seedRepository(t, r, "org_first")
	seedHistory(t, r, "org_first", "first")
	runtime := runtimeFor(t, pool, pool)
	for _, table := range []string{"collaboration_events", "collaboration_versions", "change_versions", "change_evidence", "native_commands"} {
		for _, op := range []string{"update", "delete"} {
			t.Run(table+"/"+op, func(t *testing.T) {
				tx := tenantTx(t, runtime, pool, "org_first")
				statement := "DELETE FROM " + table
				if op == "update" {
					statement = "UPDATE " + table + " SET payload_object_key=payload_object_key"
				}
				_, err := tx.ExecContext(t.Context(), statement)
				requireSQLState(t, err, "23514")
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestEventChangeSequenceContention(t *testing.T) {
	for _, kind := range []string{"event", "event replay", "evidence", "evidence replay"} {
		t.Run(kind, func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
			seedRepository(t, r, "org_first")
			f := seedHistory(t, r, "org_first", "first")
			preparedEvents := []eventstore.Event{historyEvent(t, f.content, "org_first", "contended1"), historyEvent(t, f.content, "org_first", "contended2")}
			if kind == "event replay" {
				preparedEvents[1] = preparedEvents[0]
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			ready := make(chan struct{})
			var firstSequence, secondSequence int64
			appendRecord := func(ctx context.Context, w eventstore.Writer, index int) (int64, error) {
				switch kind {
				case "event", "event replay":
					event := preparedEvents[index-1]
					got, err := w.AppendEvent(ctx, event)
					return got.Sequence, err
				default:
					evidence := f.evidence
					evidence.Sequence = 0
					evidence.SourceKey = new("contended")
					if kind == "evidence" {
						evidence.SourceKey = new(fmt.Sprint(index))
					}
					got, err := w.AppendEvidence(ctx, evidence)
					return got.Sequence, err
				}
			}
			first := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var err error
					firstSequence, err = appendRecord(ctx, w, 1)
					if err != nil {
						return err
					}
					close(ready)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
			waitRepositorySignal(t, ready)
			second := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var err error
					secondSequence, err = appendRecord(ctx, w, 2)
					return err
				})
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("second append has not reached the parent lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			if err := first.wait(t); err != nil {
				t.Fatal(err)
			}
			if err := second.wait(t); err != nil {
				t.Fatal(err)
			}
			want := firstSequence + 1
			if kind == "event replay" || kind == "evidence replay" {
				want = firstSequence
			}
			if firstSequence != 2 || secondSequence != want {
				t.Fatalf("sequences=(%d,%d), want (2,%d)", firstSequence, secondSequence, want)
			}
			if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
				switch kind {
				case "event", "event replay":
					records, err := reader.Events(t.Context(), "prj_first", "wi_first_1", 1, 10)
					if err == nil && (len(records) != int(want-1) || records[0].Sequence != 2 || records[len(records)-1].Sequence != want) {
						t.Errorf("event order=%+v", records)
					}
					return err
				default:
					records, err := reader.Evidence(t.Context(), f.change.ID, 1, 10)
					if err == nil && (len(records) != int(want-1) || records[0].Sequence != 2 || records[len(records)-1].Sequence != want) {
						t.Errorf("evidence order=%+v", records)
					}
					return err
				}
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEventChangeImportedSequenceContinuation(t *testing.T) {
	for _, kind := range []string{"event", "evidence"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedRepository(t, r, "org_first")
			f := seedHistory(t, r, "org_first", "first")
			for index, sequence := range []int64{40, 12, 0} {
				record := historyEvent(t, f.content, "org_first", fmt.Sprintf("import_%d", index))
				record.Sequence = sequence
				err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var got int64
					if kind == "event" {
						stored, err := w.AppendEvent(t.Context(), record)
						if err != nil {
							return err
						}
						got = stored.Sequence
						replay, err := w.AppendEvent(t.Context(), record)
						if err != nil {
							return err
						}
						if replay.Sequence != got {
							t.Error("import replay changed event sequence")
						}
					} else {
						record := f.evidence
						record.Sequence = sequence
						record.SourceKey = nil
						stored, err := w.AppendEvidence(t.Context(), record)
						if err != nil {
							return err
						}
						got = stored.Sequence
						if sequence != 0 {
							replay, err := w.AppendEvidence(t.Context(), record)
							if err != nil {
								return err
							}
							if replay.Sequence != got {
								t.Error("import replay changed evidence sequence")
							}
						}
					}
					want := sequence
					if sequence == 0 {
						want = 41
					}
					if got != want {
						t.Errorf("allocated=%d want=%d", got, want)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
				var sequences []int64
				if kind == "event" {
					rows, err := reader.Events(t.Context(), "prj_first", "wi_first_1", 1, 10)
					if err != nil {
						return err
					}
					for _, row := range rows {
						sequences = append(sequences, row.Sequence)
					}
				} else {
					rows, err := reader.Evidence(t.Context(), f.change.ID, 1, 10)
					if err != nil {
						return err
					}
					for _, row := range rows {
						sequences = append(sequences, row.Sequence)
					}
				}
				if !reflect.DeepEqual(sequences, []int64{12, 40, 41}) {
					t.Errorf("history order=%v", sequences)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEventChangeCommandReceiptContention(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_request_%v", changed), func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
			content := eventstore.NewContent(newHistoryObjects())
			first, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_first", ActorID: "example", Operation: "create_change", CommandKey: "concurrent", RequestHash: artifact.Digest([]byte("first"))}, []byte(`{"result":"original"}`))
			if err != nil {
				t.Fatal(err)
			}
			next, err := content.PrepareReceipt(t.Context(), first, []byte(`{"result":"later"}`))
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				next.RequestHash = artifact.Digest([]byte("different"))
			}
			ready, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var receipt eventstore.Receipt
			holder := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var err error
					first, err = w.PutReceipt(ctx, first)
					if err != nil {
						return err
					}
					close(ready)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
			waitRepositorySignal(t, ready)
			waiter := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error { var err error; receipt, err = w.PutReceipt(ctx, next); return err })
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("receipt writer has not reached the unique key lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			if err := holder.wait(t); err != nil {
				t.Fatal(err)
			}
			err = waiter.wait(t)
			if changed {
				if !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("changed hash error=%v", err)
				}
			} else if err != nil || !reflect.DeepEqual(receipt, first) {
				t.Fatalf("replayed receipt=%+v error=%v", receipt, err)
			}
			if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
				var err error
				receipt, err = reader.Receipt(t.Context(), first.ActorID, first.Operation, first.CommandKey)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			raw, err := content.Receipt(t.Context(), receipt)
			if err != nil || string(raw) != `{"result":"original"}` {
				t.Fatalf("durable receipt=%s error=%v", raw, err)
			}
		})
	}
}
