package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledSourceProjectionPreservesCanonicalScope(t *testing.T) {
	at := time.Date(2026, 11, 1, 1, 2, 3, 123456789, time.UTC)
	for _, test := range []struct {
		name, body       string
		raw              string
		invalid, refused bool
		comment, foreign bool
		numeric          bool
		numericRaw       string
		revision         int64
		revisionInvalid  bool
		unknown          bool
	}{
		{name: "issue", body: "detent-checks"},
		{name: "comment", body: "detent-checks", comment: true},
		{name: "bound numeric issue", body: "detent-checks", numeric: true},
		{name: "bound numeric comment", body: "detent-checks", comment: true, numeric: true},
		{name: "exact maximum integer revision", body: "detent-checks", numeric: true, numericRaw: "9223372036854775807", revision: 9223372036854775807},
		{name: "overflow scheduled revision", body: "detent-checks", numeric: true, numericRaw: "9223372036854775808", refused: true, revisionInvalid: true},
		{name: "fractional scheduled revision", body: "detent-checks", numeric: true, numericRaw: "2.0", refused: true, revisionInvalid: true},
		{name: "null scheduled revision", body: "detent-checks", numeric: true, numericRaw: "null", refused: true, revisionInvalid: true},
		{name: "foreign numeric issue", body: "detent-checks", foreign: true, numeric: true},
		{name: "ordinary issue", body: "ordinary"},
		{name: "numeric legacy doctor payload", raw: `{"id":"wi_example","project":"prj_example","revision":2,"state":"Todo","body":"<!-- detent:self-improvement proposal_id=example-lane -->"}`, unknown: true},
		{name: "numeric legacy comment", comment: true, raw: `{"id":"comment_example","project":"prj_example","revision":2,"body":"ordinary"}`, unknown: true},
		{name: "ordinary opaque scalar", raw: `"legacy payload"`, unknown: true},
		{name: "unrelated title marker", raw: `{"id":"wi_example","project":"prj_example","revision":2,"title":"detent-checks","body":"ordinary"}`, unknown: true},
		{name: "numeric legacy scheduled refusal", raw: `{"id":"wi_example","project":"prj_example","revision":2,"body":"detent-checks"}`, refused: true},
		{name: "escaped numeric scheduled refusal", raw: `{"revision":2,"body":"detent\u002dchecks"}`, refused: true},
		{name: "malformed scheduled JSON", raw: `{"body":"detent-checks"`, invalid: true},
		{name: "unbound ordinary source", body: "ordinary", foreign: true, unknown: true},
		{name: "foreign issue", body: "detent-checks", foreign: true},
		{name: "foreign comment", body: "detent-checks", comment: true, foreign: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := CollaborationVersion{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", RecordID: "wi_example", Revision: 2}
			if test.revision != 0 {
				row.Revision = test.revision
			}
			organization := row.OrganizationID
			if test.foreign {
				organization = "org_other"
			}
			var value any = tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, Revision: tracker.Revision(row.Revision)}, Body: test.body, UpdatedAt: at}
			if test.comment {
				row.RecordID = "comment_example"
				value = tracker.NativeComment{OrganizationID: organization, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ID: row.RecordID, Revision: tracker.Revision(row.Revision), Body: test.body, UpdatedAt: at}
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if test.numeric {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				number := test.numericRaw
				if number == "" {
					number = "2"
				}
				fields["revision"] = json.RawMessage(number)
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.raw != "" {
				raw = []byte(test.raw)
			}
			before := bytes.Clone(raw)
			projected, err := ProjectScheduledSource(row, raw)
			if !bytes.Equal(raw, before) {
				t.Fatal("classification changed original payload bytes")
			}
			if test.invalid {
				if !errors.Is(err, ErrInvalidRecord) {
					t.Fatal("malformed JSON was accepted as opaque history")
				}
				return
			}
			if test.unknown {
				if err != nil || projected.SourceKnown {
					t.Fatal("unbound body became known nonscheduled evidence")
				}
				return
			}
			if test.foreign || test.refused {
				if test.revisionInvalid {
					_, decodeErr := DecodeCollaborationIssue(row, raw)
					if !errors.Is(decodeErr, ErrInvalidRecord) {
						t.Fatal("non-integer revision was coerced")
					}
				}
				if !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatal("foreign scheduled body was accepted")
				}
				return
			}
			if test.numeric {
				var currentWrite any = &tracker.NativeIssue{}
				if test.comment {
					currentWrite = &tracker.NativeComment{}
				}
				if json.Unmarshal(raw, currentWrite) == nil {
					t.Fatal("history compatibility changed current native revision decoder")
				}
				if test.comment {
					decoded, decodeErr := DecodeCollaborationComment(row, raw)
					if decodeErr != nil || int64(decoded.Revision) != row.Revision || decoded.Body != test.body {
						t.Fatalf("numeric comment read lost source: %v", decodeErr)
					}
				} else {
					decoded, decodeErr := DecodeCollaborationIssue(row, raw)
					if decodeErr != nil || int64(decoded.Revision) != row.Revision || decoded.Body != test.body {
						t.Fatalf("numeric issue read lost source: %v", decodeErr)
					}
				}
				if !bytes.Equal(raw, before) {
					t.Fatal("numeric history read rewrote stored payload")
				}
			}
			if err != nil || !projected.SourceKnown || projected.SourceScheduled != (test.body == "detent-checks") || !projected.SourceObservedAt.Equal(time.Date(2026, 11, 1, 1, 2, 3, 123456000, time.UTC)) {
				t.Fatalf("scheduled classification=%+v: %v", projected, err)
			}
		})
	}
}
