package tracker

import (
	"encoding/json"
	"errors"
	"testing"
)

// A patch has three things to say about a priority. The encoding has to keep
// all three apart, because the middle one — a clear — is the whole reason the
// type exists.
func TestPriorityPatchDecodesThreeIntents(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		body        string
		wantPresent bool
		wantLevel   *int
		wantErr     bool
	}{
		{name: "absent", body: `{"expected_revision":"1"}`},
		{name: "a level", body: `{"expected_revision":"1","priority":2}`, wantPresent: true, wantLevel: level(2)},
		{name: "the lowest level", body: `{"expected_revision":"1","priority":0}`, wantPresent: true, wantLevel: level(0)},
		{name: "a json null", body: `{"expected_revision":"1","priority":null}`, wantPresent: true},
		{name: "the word none", body: `{"expected_revision":"1","priority":"none"}`, wantPresent: true},
		{name: "another word", body: `{"expected_revision":"1","priority":"urgent"}`, wantErr: true},
		{name: "the word in another case", body: `{"expected_revision":"1","priority":"None"}`, wantErr: true},
		{name: "an object", body: `{"expected_revision":"1","priority":{}}`, wantErr: true},
		{name: "a fraction", body: `{"expected_revision":"1","priority":1.5}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request UpdateIssue
			err := json.Unmarshal([]byte(test.body), &request)
			if (err != nil) != test.wantErr {
				t.Fatalf("Unmarshal(%s) error = %v, want error %v", test.body, err, test.wantErr)
			}
			if test.wantErr {
				if !errors.Is(err, ErrInvalidPriorityPatch) {
					t.Fatalf("error = %v, want ErrInvalidPriorityPatch", err)
				}
				return
			}
			if request.Priority.Present() != test.wantPresent {
				t.Fatalf("Present() = %v, want %v", request.Priority.Present(), test.wantPresent)
			}
			got := request.Priority.Level()
			switch {
			case test.wantLevel == nil && got != nil:
				t.Fatalf("Level() = %d, want none", *got)
			case test.wantLevel != nil && got == nil:
				t.Fatalf("Level() = none, want %d", *test.wantLevel)
			case test.wantLevel != nil && *got != *test.wantLevel:
				t.Fatalf("Level() = %d, want %d", *got, *test.wantLevel)
			}
		})
	}
}

func TestPriorityPatchConstructors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		patch       PriorityPatch
		wantPresent bool
		wantLevel   *int
	}{
		{name: "leave", patch: LeavePriority()},
		{name: "the zero value leaves too", patch: PriorityPatch{}},
		{name: "clear", patch: ClearPriority(), wantPresent: true},
		{name: "set", patch: SetPriority(level(3)), wantPresent: true, wantLevel: level(3)},
		// A nil level is a caller with nothing to say, not a clear: the two
		// Go callers that patch a priority pass an optional pointer through.
		{name: "set nothing", patch: SetPriority(nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.patch.Present() != test.wantPresent {
				t.Fatalf("Present() = %v, want %v", test.patch.Present(), test.wantPresent)
			}
			got := test.patch.Level()
			switch {
			case test.wantLevel == nil && got != nil:
				t.Fatalf("Level() = %d, want none", *got)
			case test.wantLevel != nil && got == nil:
				t.Fatalf("Level() = none, want %d", *test.wantLevel)
			case test.wantLevel != nil && *got != *test.wantLevel:
				t.Fatalf("Level() = %d, want %d", *got, *test.wantLevel)
			}
			// Level hands back a copy, so a caller cannot reach into the
			// patch and change what the request said.
			if got != nil {
				*got = 99
				if again := test.patch.Level(); again == nil || *again != *test.wantLevel {
					t.Fatalf("Level() returned a live pointer into the patch")
				}
			}
		})
	}
}

func TestPriorityPatchMarshals(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		patch PriorityPatch
		want  string
	}{
		{name: "leave", patch: LeavePriority(), want: "null"},
		{name: "clear", patch: ClearPriority(), want: "null"},
		{name: "set", patch: SetPriority(level(1)), want: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.patch)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("Marshal() = %s, want %s", encoded, test.want)
			}
		})
	}
}

func level(value int) *int { return &value }

func TestUpdateIssueRoundTripsPriorityIntent(t *testing.T) {
	t.Parallel()
	title := "renamed"
	for _, test := range []struct {
		name        string
		request     UpdateIssue
		wantMember  bool
		wantPresent bool
		wantLevel   *int
	}{
		{name: "an unrelated edit leaves the priority", request: UpdateIssue{ExpectedRevision: 1, Title: &title}},
		{name: "a clear", request: UpdateIssue{ExpectedRevision: 1, Priority: ClearPriority()}, wantMember: true, wantPresent: true},
		{name: "a level", request: UpdateIssue{ExpectedRevision: 1, Priority: SetPriority(level(3))}, wantMember: true, wantPresent: true, wantLevel: level(3)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &members); err != nil {
				t.Fatal(err)
			}
			if _, ok := members["priority"]; ok != test.wantMember {
				t.Fatalf("encoded %s: priority member present = %v, want %v", encoded, ok, test.wantMember)
			}
			var decoded UpdateIssue
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Priority.Present() != test.wantPresent {
				t.Fatalf("Present() = %v, want %v", decoded.Priority.Present(), test.wantPresent)
			}
			got := decoded.Priority.Level()
			if (got == nil) != (test.wantLevel == nil) || (got != nil && *got != *test.wantLevel) {
				t.Fatalf("Level() = %v, want %v", got, test.wantLevel)
			}
		})
	}
}
