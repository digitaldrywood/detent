package hubrecords

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type failingDetailStore struct{ err error }

func (f failingDetailStore) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return attachment.ArchiveReference{}, f.err
}

func (f failingDetailStore) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, f.err
}

func TestRunEventDetailStoreReadClassifiesFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		err         error
		wantMissing bool
		wantStorage bool
	}{
		{"missing object", artifact.ErrMissing, true, false},
		{"untyped store failure", errors.New("connection reset"), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := RunEventDetailStore{DetailStore: failingDetailStore{err: test.err}}.ReadDetail(t.Context(), "org_example", attachment.ArchiveReference{})
			if errors.Is(err, artifact.ErrMissing) != test.wantMissing || errors.Is(err, artifact.ErrStorage) != test.wantStorage {
				t.Fatalf("ReadDetail error = %v, want missing=%t storage=%t", err, test.wantMissing, test.wantStorage)
			}
		})
	}
}

func TestNativeRunDetailRetainsHostLandingTransition(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"", "Rework", "Done"} {
		t.Run(target, func(t *testing.T) {
			archived := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", LeaseID: "lease", FencingToken: 1, Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{VersionID: "version", FromState: "old", Gate: &gate.CommandResult{Output: "retained output"}}}}
			stored := StoredRun{NativeRunData: CompactRun(archived), Detail: &RunDetailReference{AttachmentID: "attachment", Entry: "attempt:0"}}
			stored.Runtime.Landing.FromState, stored.Runtime.Landing.TargetState = "Merging", target
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), hubcontent.RunDetailContextKey{}, &hubcontent.RunDetailContext{Cache: map[string]map[string]tracker.NativeRunData{"attachment": {"attempt:0": archived}}})
			var got tracker.NativeRunData
			if err := DecodeRunDetail(ctx, nil, string(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got.Runtime.Landing.FromState != "Merging" || got.Runtime.Landing.TargetState != target || got.Runtime.Landing.Gate.Output != "retained output" {
				t.Fatalf("hydrated landing lost host transition or archive output: %+v", got.Runtime.Landing)
			}
			if archived.Runtime.Landing.FromState != "old" || archived.Runtime.Landing.TargetState != "" {
				t.Fatal("hydration mutated cached archive")
			}
		})
	}
}

func TestRunDetailSegmentResolvesPreparedReferenceBeforeCommit(t *testing.T) {
	t.Parallel()
	store := hubtest.NewDetails()
	organization := "org_prepared"
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	prepared := map[string]tracker.NativeRunData{"attempt_prepared:2": {AttemptID: "attempt_prepared", Sequence: 2}}
	owner := &hubcontent.RunDetailContext{Store: store, Organization: organization, Cache: map[string]map[string]tracker.NativeRunData{}, Reference: &ref, Runs: prepared}
	ctx := context.WithValue(t.Context(), hubcontent.RunDetailContextKey{}, owner)
	runs, err := ReadRunDetailSegment(ctx, nil, NewRunDetailReference(ref, organization, "attempt_prepared:2"))
	if err != nil || runs["attempt_prepared:2"].Sequence != 2 {
		t.Fatalf("prepared detail before commit = %v, %v", runs, err)
	}
}

func TestRunDetailSegmentRejectsLegacyArchiveWithoutDatabaseReader(t *testing.T) {
	t.Parallel()
	store := hubtest.NewDetails()
	organization := "org_archive"
	owner := &hubcontent.RunDetailContext{Store: store, Organization: organization, Cache: map[string]map[string]tracker.NativeRunData{}}
	ctx := context.WithValue(t.Context(), hubcontent.RunDetailContextKey{}, owner)
	detail := &RunDetailReference{AttachmentID: "att_legacy", Organization: organization, Entry: "attempt_legacy:1"}
	if _, err := ReadRunDetailSegment(ctx, nil, detail); !errors.Is(err, ErrRunDetailStoreUnavailable) {
		t.Fatalf("legacy archive without typed owner error = %v", err)
	}
}
