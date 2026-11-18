package hubrecords

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptAttachmentReader struct {
	tenantstore.Reader
	row attachmentstore.Record
}

func (r attemptAttachmentReader) Attachment(context.Context, tracker.ProjectID, string) (attachmentstore.Record, error) {
	return r.row, nil
}

func TestNativeAttemptAttachmentPayloadIntegrity(t *testing.T) {
	t.Parallel()
	ref, _, err := attachment.PrepareArchive("org_example", 1, strings.NewReader(`{"sequence":43}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*attachmentstore.Record)
	}{
		{"valid", nil},
		{"organization", func(r *attachmentstore.Record) { r.OrganizationID = "org_other" }},
		{"project", func(r *attachmentstore.Record) { r.Metadata.ProjectID = "prj_other" }},
		{"attachment", func(r *attachmentstore.Record) { r.Metadata.ID = "att_other" }},
		{"key", func(r *attachmentstore.Record) { r.ObjectKey += "wrong" }},
		{"digest", func(r *attachmentstore.Record) { r.Metadata.SHA256 = strings.Repeat("f", 64) }},
		{"size", func(r *attachmentstore.Record) { r.Metadata.Size = 0 }},
		{"content size", func(r *attachmentstore.Record) { r.UncompressedSize = 0 }},
		{"codec", func(r *attachmentstore.Record) { r.Codec = "plain" }},
		{"schema", func(r *attachmentstore.Record) { r.SchemaVersion = 2 }},
		{"kind", func(r *attachmentstore.Record) { r.Kind = "attachment" }},
		{"retention", func(r *attachmentstore.Record) { r.Retention = "temporary" }},
		{"binding", func(r *attachmentstore.Record) { r.StorageBindingID = "binding_example" }},
		{"customer owned", func(r *attachmentstore.Record) { r.CustomerOwned = true }},
		{"deleted", func(r *attachmentstore.Record) { r.Metadata.DeletedAt = new(time.Time) }},
		{"object deleted", func(r *attachmentstore.Record) { r.ObjectDeletedAt = new(time.Time) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := TenantAttemptSnapshot{Attempt: leasestore.Attempt{OrganizationID: "org_example", PayloadObjectKey: ref.Key}, Event: eventstore.Event{ProjectID: "prj_example"}}
			row := attachmentstore.Record{OrganizationID: "org_example", ObjectKey: ref.Key, UncompressedSize: ref.UncompressedSize,
				Codec: ref.Codec, SchemaVersion: 1, Kind: ref.Kind, Retention: ref.Retention,
				Metadata: attachment.Metadata{ID: NewRunDetailReference(ref, "org_example", "").AttachmentID, ProjectID: "prj_example", SHA256: ref.SHA256, Size: ref.CompressedSize}}
			if test.change != nil {
				test.change(&row)
			}
			err := ReadTenantAttemptAttachmentPayload(t.Context(), attemptAttachmentReader{row: row}, &snapshot)
			if test.change == nil {
				if err != nil || snapshot.Payload == nil || *snapshot.Payload != ref {
					t.Fatalf("canonical reference = %+v: %v", snapshot.Payload, err)
				}
			} else if !errors.Is(err, artifact.ErrIntegrity) || snapshot.Payload != nil {
				t.Fatalf("invalid metadata accepted: reference=%+v error=%v", snapshot.Payload, err)
			}
		})
	}
}
