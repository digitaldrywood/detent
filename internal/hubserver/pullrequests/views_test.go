package pullrequests

import (
	"encoding/json"
	"fmt"
	"github.com/digitaldrywood/detent/internal/tracker"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPullRequestMergeableJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		want  string
	}{
		{name: "clean", state: "clean", want: "true"},
		{name: "blocked is still mergeable as far as the projection knows", state: "blocked", want: "true"},
		{name: "dirty", state: "dirty", want: "false"},
		{name: "empty", state: "", want: `"unknown"`},
		{name: "unknown", state: "UNKNOWN", want: `"unknown"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(MergeableFromState(test.state))
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("mergeable = %s, want %s", encoded, test.want)
			}
			var back Mergeable
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("decode %s: %v", encoded, err)
			}
			if back != MergeableFromState(test.state) {
				t.Fatalf("round trip = %#v", back)
			}
			if err := json.Unmarshal([]byte(`"maybe"`), &back); err == nil {
				t.Fatal("an unknown string must be refused")
			}
		})
	}
}

func TestExternalPullRequestNumber(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		reference tracker.ChangeExternalReference
		want      int
	}{
		{name: "github number", reference: tracker.ChangeExternalReference{Provider: "github", ID: "42"}, want: 42},
		{name: "padded provider and id", reference: tracker.ChangeExternalReference{Provider: " GitHub ", ID: " 7 "}, want: 7},
		{name: "node id", reference: tracker.ChangeExternalReference{Provider: "github", ID: "PR_kwDO"}},
		{name: "zero", reference: tracker.ChangeExternalReference{Provider: "github", ID: "0"}},
		{name: "negative", reference: tracker.ChangeExternalReference{Provider: "github", ID: "-3"}},
		{name: "other provider", reference: tracker.ChangeExternalReference{Provider: "gitlab", ID: "42"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ExternalNumber(test.reference); got != test.want {
				t.Fatalf("ExternalNumber(%#v) = %d, want %d", test.reference, got, test.want)
			}
		})
	}
}

// The assembled view is served from memory for sixty seconds, so a client that
// polls the panel does not re-run the join on every tick.
func TestPullRequestViewCache(t *testing.T) {
	cache := NewCache()
	now := time.Now()
	views := []View{{ID: "change", State: "open"}}
	cache.Set("issue", views, now)
	views[0].State = "closed"
	cached, ok := cache.Get("issue", now)
	if !ok || len(cached) != 1 || cached[0].State != "open" {
		t.Fatalf("cache changed: %+v", cached)
	}
	if _, ok := cache.Get("issue", now.Add(ViewTTL)); ok {
		t.Fatal("expired view retained")
	}
}

func TestPullRequestViewGoldenShape(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "pull-requests.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var views []View
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&views); err != nil {
		t.Fatalf("the golden must decode into the served shape: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("golden views = %d, want 2", len(views))
	}
	if views[0].Connector == nil || views[0].Number != 4211 || !views[0].Mergeable.Known || !views[0].Mergeable.Value {
		t.Fatalf("connected row = %#v", views[0])
	}
	if views[1].Connector != nil || views[1].Mergeable.Known {
		t.Fatalf("unavailable row = %#v", views[1])
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	comparePullRequestShape(t, "", want, got)
}

// comparePullRequestShape asserts that the Go payload carries exactly the
// golden's key set at every level and agrees on whether a value is null.
// Values themselves are not compared: the golden is an example, the shape is
// the contract.
func comparePullRequestShape(t *testing.T, path string, want, got any) {
	t.Helper()
	label := path
	if label == "" {
		label = "(root)"
	}
	if (want == nil) != (got == nil) {
		t.Errorf("%s: golden null = %t, Go null = %t", label, want == nil, got == nil)
		return
	}
	switch wanted := want.(type) {
	case nil:
	case map[string]any:
		gotten, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: golden is an object, Go value is %T", label, got)
			return
		}
		for key, value := range wanted {
			other, present := gotten[key]
			if !present {
				t.Errorf("%s: Go value is missing key %q", label, key)
				continue
			}
			comparePullRequestShape(t, strings.TrimPrefix(path+"."+key, "."), value, other)
		}
		for key := range gotten {
			if _, present := wanted[key]; !present {
				t.Errorf("%s: Go value has extra key %q", label, key)
			}
		}
	case []any:
		gotten, ok := got.([]any)
		if !ok {
			t.Errorf("%s: golden is an array, Go value is %T", label, got)
			return
		}
		if len(wanted) != len(gotten) {
			t.Errorf("%s: golden has %d elements, Go value has %d", label, len(wanted), len(gotten))
			return
		}
		for i := range wanted {
			comparePullRequestShape(t, path+"[]", wanted[i], gotten[i])
		}
	default:
		if fmt.Sprintf("%T", want) != fmt.Sprintf("%T", got) {
			t.Errorf("%s: golden is %T, Go value is %T", label, want, got)
		}
	}
}
