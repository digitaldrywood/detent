package eventstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type failingContentReader struct {
	attachment.DetailStore
	failure   error
	reference attachment.ArchiveReference
}

func (s failingContentReader) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return s.reference, nil
}

func (s failingContentReader) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, s.failure
}

func TestContentReadPreservesStorageFailureClass(t *testing.T) {
	digest := artifact.Digest([]byte(`{}`))
	key, err := attachment.ArchiveKey("org_example", digest)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		failure   error
		integrity bool
	}{
		{"backend", artifact.ErrStorage, false},
		{"missing", artifact.ErrMissing, true},
		{"not exist", fs.ErrNotExist, true},
		{"integrity", artifact.ErrIntegrity, true},
		{"cancelled", context.Canceled, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := NewContent(failingContentReader{failure: test.failure, reference: Reference(key, digest, 10, 2)})
			_, err := content.CollaborationVersion(t.Context(), CollaborationVersion{OrganizationID: "org_example", PayloadObjectKey: key, PayloadSHA256: digest, PayloadBytes: 10, PayloadContentBytes: 2})
			if !errors.Is(err, test.failure) || errors.Is(err, artifact.ErrIntegrity) != test.integrity {
				t.Fatalf("read failure=%v, want %v integrity=%t", err, test.failure, test.integrity)
			}
			_, err = content.PreparePayload(t.Context(), "org_example", []byte(`{}`))
			if !errors.Is(err, test.failure) || errors.Is(err, artifact.ErrIntegrity) != test.integrity {
				t.Fatalf("verification failure=%v, want %v integrity=%t", err, test.failure, test.integrity)
			}
		})
	}
}

type versionContentStore struct {
	attachment.DetailStore
	raw []byte
}

func (s *versionContentStore) StoreDetail(_ context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	s.raw = bytes.Clone(raw)
	digest := artifact.Digest(raw)
	key, err := attachment.ArchiveKey(organization, digest)
	return Reference(key, digest, int64(len(raw)), int64(len(raw))), err
}

func (s *versionContentStore) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return bytes.Clone(s.raw), nil
}

func TestCollaborationVersionPreservesHistoricalBytes(t *testing.T) {
	for _, raw := range []string{"  { \"body\": \"<value>\" }\n", "{\"body\":\"雪\",\"value\":1.00}"} {
		t.Run(raw, func(t *testing.T) {
			content := NewContent(&versionContentStore{})
			row, err := content.PrepareCollaborationVersion(t.Context(), CollaborationVersion{OrganizationID: "org_example"}, []byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			stored, err := content.CollaborationVersion(t.Context(), row)
			if err != nil || string(stored) != raw || row.PayloadContentBytes != int64(len(raw)) {
				t.Fatalf("historical bytes changed: got %q length=%d error=%v, want %q", stored, row.PayloadContentBytes, err, raw)
			}
		})
	}
}

type archiveSourceStore struct {
	attachment.ArchiveStorage
	objects map[string][]byte
}

func (s *archiveSourceStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	object, found := s.objects[key]
	if !found {
		return nil, artifact.ErrMissing
	}
	return io.NopCloser(bytes.NewReader(object)), nil
}

func (s *archiveSourceStore) StoreDetail(_ context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	s.objects[ref.Key] = compressed
	return ref, nil
}

func (s *archiveSourceStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return attachment.ReadArchive(ctx, s, organization, ref)
}

func (s *archiveSourceStore) put(t *testing.T, organization string, object []byte, content int64) attachment.ArchiveReference {
	t.Helper()
	digest := artifact.Digest(object)
	key, err := attachment.ArchiveKey(organization, digest)
	if err != nil {
		t.Fatal(err)
	}
	s.objects[key] = object
	return Reference(key, digest, int64(len(object)), content)
}

func sourceBundleFixture(content []byte) (tracker.ChangeSource, []byte) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	bundle := append([]byte("# v2 git bundle\n-"+base+" base\n"+head+" refs/heads/main\n\n"), content...)
	return tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("c", 64), Bytes: int64(len(bundle))}, bundle
}

func sourceBundleRow(source tracker.ChangeSource, ref attachment.ArchiveReference) Version {
	return Version{OrganizationID: "org_example", BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA, SourceObjectKey: ref.Key, SourceSHA256: ref.SHA256, SourceBytes: ref.CompressedSize, SourceContentBytes: ref.UncompressedSize}
}

func TestSourceBundleRoundTrip(t *testing.T) {
	t.Parallel()
	random := rand.New(rand.NewPCG(1, 2))
	binaryContent := make([]byte, 128<<10)
	for offset := 0; offset < len(binaryContent); offset += 8 {
		binary.LittleEndian.PutUint64(binaryContent[offset:], random.Uint64())
	}
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{"small", []byte("reviewed source\n")},
		{"text", bytes.Repeat([]byte("retained source\n"), 100000)},
		{"empty delta", nil},
		{"binary", binaryContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			content := NewContent(&archiveSourceStore{objects: map[string][]byte{}})
			source, bundle := sourceBundleFixture(test.content)
			ref, err := content.PrepareSourceBundle(t.Context(), "org_example", bundle)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := content.SourceBundle(t.Context(), "org_example", sourceBundleRow(source, ref), source)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(restored, bundle) {
				t.Fatal("source round trip changed bytes")
			}
			if err := source.Validate(source.BaseSHA, source.HeadSHA, restored); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourceBundleRefusesCorruptionAndExpansion(t *testing.T) {
	t.Parallel()
	const secret = "private-bundle-content"
	store := &archiveSourceStore{objects: map[string][]byte{}}
	content := NewContent(store)
	source, bundle := sourceBundleFixture([]byte(secret))
	ref, err := content.PrepareSourceBundle(t.Context(), "org_example", bundle)
	if err != nil {
		t.Fatal(err)
	}
	valid := bytes.Clone(store.objects[ref.Key])
	size := int64(len(bundle))
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-1] ^= 255
	expandedSource, expanded := sourceBundleFixture(bytes.Repeat([]byte("x"), tracker.MaxChangeSourceBytes+1))
	expandedRef, expandedObject, err := attachment.PrepareArchive("org_example", 1, bytes.NewReader(expanded))
	if err != nil {
		t.Fatal(err)
	}
	expandedSource.Bytes = tracker.MaxChangeSourceBytes
	tampered := func(object []byte) func(*testing.T) (Version, tracker.ChangeSource) {
		return func(t *testing.T) (Version, tracker.ChangeSource) {
			t.Helper()
			return sourceBundleRow(source, store.put(t, "org_example", object, size)), source
		}
	}
	replaced := func(object []byte) func(*testing.T) (Version, tracker.ChangeSource) {
		return func(t *testing.T) (Version, tracker.ChangeSource) {
			t.Helper()
			store.objects[ref.Key] = object
			t.Cleanup(func() { store.objects[ref.Key] = valid })
			return sourceBundleRow(source, ref), source
		}
	}
	recorded := func(contentBytes, sourceBytes int64) func(*testing.T) (Version, tracker.ChangeSource) {
		return func(*testing.T) (Version, tracker.ChangeSource) {
			row, changed := sourceBundleRow(source, ref), source
			row.SourceContentBytes, changed.Bytes = contentBytes, sourceBytes
			return row, changed
		}
	}
	for _, test := range []struct {
		name  string
		stage func(*testing.T) (Version, tracker.ChangeSource)
	}{
		{"invalid gzip", tampered([]byte(secret))},
		{"invalid gzip in place", replaced([]byte(secret))},
		{"truncated gzip", tampered(valid[:len(valid)-1])},
		{"truncated gzip in place", replaced(valid[:len(valid)-1])},
		{"checksum", tampered(corrupt)},
		{"checksum in place", replaced(corrupt)},
		{"trailing data", tampered(append(bytes.Clone(valid), secret...))},
		{"trailing data in place", replaced(append(bytes.Clone(valid), secret...))},
		{"concatenated streams", tampered(append(bytes.Clone(valid), valid...))},
		{"concatenated streams in place", replaced(append(bytes.Clone(valid), valid...))},
		{"recorded bound", recorded(size-1, size-1)},
		{"recorded source bound", recorded(size, size-1)},
		{"short output", recorded(size+1, size+1)},
		{"short source output", recorded(size, size+1)},
		{"bundle bound", func(t *testing.T) (Version, tracker.ChangeSource) {
			return sourceBundleRow(expandedSource, store.put(t, "org_example", expandedObject, expandedRef.UncompressedSize)), expandedSource
		}},
		{"invalid recorded bound", recorded(size, tracker.MaxChangeSourceBytes+1)},
		{"invalid recorded size", recorded(0, 0)},
		{"invalid recorded source size", recorded(size, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			row, staged := test.stage(t)
			if restored, err := content.SourceBundle(t.Context(), "org_example", row, staged); err == nil || restored != nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("expected content-free refusal, got %d bytes, %v", len(restored), err)
			}
		})
	}
	if _, err := content.PrepareSourceBundle(t.Context(), "org_example", expanded); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected bounded refusal, got %v", err)
	}
}

func TestReviewCompatibilityOfMatchesStoredCheckSet(t *testing.T) {
	check := tracker.ChangeCheckSpec{Name: "ci", PrincipalID: "ci", WorkflowID: "ci.yml", WorkflowSHA256: strings.Repeat("a", 64), Source: "independent", MaxAgeSeconds: 3600}
	for _, tc := range []struct {
		name   string
		checks []tracker.ChangeCheckSpec
		want   []tracker.ChangeCheckSpec
	}{
		{name: "absent"},
		{name: "known empty", checks: []tracker.ChangeCheckSpec{}},
		{name: "listed", checks: []tracker.ChangeCheckSpec{check}, want: []tracker.ChangeCheckSpec{check}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := ReviewCompatibilityOf(tracker.ChangeReviewPolicy{RequireReview: true, RequiredChecks: tc.checks})
			if facts == nil || !facts.RequireReview || !reflect.DeepEqual(facts.RequiredChecks, tc.want) {
				t.Fatalf("facts = %+v, want require review with checks %#v", facts, tc.want)
			}
		})
	}
}
