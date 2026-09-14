package connector

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestNativeWorkpadAuthority(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, source string
		refs         []BlockedRef
		want         int
	}{
		{name: "native removed", source: BlockedRefSourceNative, want: 1},
		{name: "native present", source: BlockedRefSourceNative, refs: []BlockedRef{{Identifier: "owner/repo#100", Source: BlockedRefSourceNative}}, want: 2},
		{name: "prose fallback", source: BlockedRefSourceProse, want: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := Issue{Identifier: "owner/repo#101", DependencySource: tt.source, BlockedBy: tt.refs, WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "approve access", Blockers: []workpad.Blocker{
				{Ref: "#100", Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, States: []string{"open"}}},
				{Ref: "#200", Predicate: &workpad.Predicate{Type: workpad.PredicateCheckPresence}},
			}}}
			got := issue.WithNativeWorkpadAuthority()
			if len(got.WorkpadSignal.Blockers) != tt.want || got.WorkpadSignal.HumanAction != "approve access" || got.WorkpadSignal.Status != workpad.StatusBlocked {
				t.Fatalf("signal = %+v", got.WorkpadSignal)
			}
			if len(issue.WorkpadSignal.Blockers) != 2 {
				t.Fatal("mutated original signal")
			}
			if tt.want == 1 && !strings.Contains(strings.Join(got.DependencyNotes, " "), "owner/repo#100: workpad dependency ignored: native relation absent") {
				t.Fatalf("ignored notes = %v", got.DependencyNotes)
			}
			if len(got.WithNativeWorkpadAuthority().DependencyNotes) != len(got.DependencyNotes) {
				t.Fatal("duplicate notes")
			}
		})
	}
}

func TestNativeAuthorityUsesPredicateTarget(t *testing.T) {
	t.Parallel()
	for _, nativeRef := range []string{"owner/repo#100", "owner/repo#200"} {
		t.Run(nativeRef, func(t *testing.T) {
			issue := Issue{Identifier: "owner/repo#101", DependencySource: BlockedRefSourceNative, BlockedBy: []BlockedRef{{Identifier: nativeRef}}}
			blocker := workpad.Blocker{Identifier: "owner/repo#100", Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Identifier: "owner/repo#200"}}
			got := issue.IgnoredNativeWorkpadDependency(blocker)
			want := ""
			if nativeRef == "owner/repo#100" {
				want = "owner/repo#200"
			}
			if got != want {
				t.Fatalf("ignored=%q, want %q", got, want)
			}
		})
	}
}
