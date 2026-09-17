package dependencyline

import (
	"reflect"
	"strings"
	"testing"
)

func TestDependencyAppendPreservesBody(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body, ref string
		invalid         bool
	}{
		{name: "empty declaration with prose", body: "Depends on: none. Order: #2, #3", ref: "#3"},
		{name: "and separated sentence", body: "Depends on: #2 and #3. See #9", ref: "#3"},
		{name: "append", body: "Acceptance criteria\n\nDepends on: #2\n", ref: "owner/repo#3"},
		{name: "existing", body: "Text\n- **Depends on:** #3, owner/repo#2\n", ref: "https://github.com/owner/repo/issues/3"},
		{name: "code example", body: "```text\nDepends on: #3\n```", ref: "owner/repo#3"},
		{name: "nested code example", body: "````text\n```text\nDepends on: #3\n```\n````", ref: "owner/repo#3"},
		{name: "unterminated example", body: "```text\nDepends on: #3", ref: "owner/repo#3", invalid: true},
		{name: "malformed ref", ref: "#3bad", invalid: true},
		{name: "malformed existing", body: "Depends on: #invalid", ref: "#3", invalid: true},
		{name: "overflow", ref: "#999999999999999999999999999999999", invalid: true},
		{name: "zero", ref: "#0", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Append(tt.body, "owner/repo", tt.ref)
			if (err != nil) != tt.invalid {
				t.Fatalf("Append error = %v", err)
			}
			if err != nil {
				return
			}
			if !strings.HasPrefix(got, tt.body) {
				t.Fatalf("body changed: %q", got)
			}
			again, err := Append(got, "owner/repo", tt.ref)
			if err != nil || again != got {
				t.Fatalf("retry changed body: %q, %v", again, err)
			}
		})
	}
	refs, err := References("Depends on: #2; Owner/Repo#3\nBlocked by: https://github.com/owner/repo/issues/2", "owner/repo")
	if err != nil || !reflect.DeepEqual(refs, []string{"owner/repo#2", "owner/repo#3"}) {
		t.Fatalf("refs = %v, %v", refs, err)
	}
}

func FuzzDependencyReferences(f *testing.F) {
	for _, body := range []string{"Depends on: owner/repo#1 #2", "Depends on: #1bad", "Depends on: https://github.com/owner/repo/issues/0", "```detent-human\nschema: 1\n```\nDepends on: #3"} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		got, err := Append(body, "owner/repo", "owner/repo#42")
		if err != nil {
			return
		}
		again, err := Append(got, "owner/repo", "#42")
		if err != nil || again != got || !strings.HasPrefix(got, body) {
			t.Fatalf("append not idempotent: %q, %v", again, err)
		}
	})
}

func TestDeclarationsOutsideFences(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		want       []string
		wantErr    bool
	}{
		{name: "trailing prose", body: "**Depends on:** #1 so helpers exist", want: []string{"#1"}},
		{name: "surrounding declarations", body: "Depends on: #1\n~~~text\nDepends on: #2\n~~~\nBlocked by: #3", want: []string{"#1", "#3"}},
		{name: "unfinished fence", body: "Depends on: #1\n```text\nDepends on: #2", want: []string{"#1"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, complete := Declarations(tt.body)
			if !reflect.DeepEqual(got, tt.want) || !complete != tt.wantErr {
				t.Fatalf("Declarations() = %v, %v; want %v, complete %v", got, complete, tt.want, !tt.wantErr)
			}
		})
	}
}

func TestLinesOutsideFences(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		want       []string
		complete   bool
	}{
		{name: "backticks", body: "before\n```go\nhidden\n```\nafter", want: []string{"before", "after"}, complete: true},
		{name: "tildes and CRLF", body: "before\r\n~~~\r\nhidden\r\n~~~\r\nafter", want: []string{"before", "after"}, complete: true},
		{name: "CR declarations", body: "Depends on: #1\rDepends on: #2", want: []string{"Depends on: #1", "Depends on: #2"}, complete: true},
		{name: "mismatched and short closers", body: "````\n~~~\n```\nhidden\n`````\nafter", want: []string{"after"}, complete: true},
		{name: "unfinished", body: "before\n~~~\nhidden", want: []string{"before"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, complete := LinesOutsideFences(tt.body)
			if !reflect.DeepEqual(got, tt.want) || complete != tt.complete {
				t.Fatalf("got %v, %v; want %v, %v", got, complete, tt.want, tt.complete)
			}
		})
	}
}
