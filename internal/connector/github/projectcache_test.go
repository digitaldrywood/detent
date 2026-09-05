package github

import (
	"testing"
	"time"
)

func TestProjectCacheReturnsFreshItemIDByProjectAndIssue(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.SetItemID("PVT_1", "I_1", "PVTI_1")
	cache.SetItemID("PVT_2", "I_1", "PVTI_2")

	got, ok := cache.GetItemID("PVT_1", "I_1")
	if !ok {
		t.Fatal("GetItemID() ok = false, want true")
	}
	if got != "PVTI_1" {
		t.Fatalf("itemID = %q, want PVTI_1", got)
	}
}

func TestProjectCacheExpiresItemID(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.SetItemID("PVT_1", "I_1", "PVTI_1")

	now = now.Add(5*time.Minute - time.Nanosecond)
	if _, ok := cache.GetItemID("PVT_1", "I_1"); !ok {
		t.Fatal("GetItemID() ok = false before TTL, want true")
	}

	now = now.Add(time.Nanosecond)
	if _, ok := cache.GetItemID("PVT_1", "I_1"); ok {
		t.Fatal("GetItemID() ok = true after TTL, want false")
	}
}

func TestProjectCacheClearsEntries(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.SetItemID("PVT_1", "I_1", "PVTI_1")
	cache.SetItemID("PVT_1", "I_2", "PVTI_2")

	cache.ClearItemID("PVT_1", "I_1")
	if _, ok := cache.GetItemID("PVT_1", "I_1"); ok {
		t.Fatal("GetItemID() ok = true after ClearItemID(), want false")
	}
	if _, ok := cache.GetItemID("PVT_1", "I_2"); !ok {
		t.Fatal("GetItemID() ok = false for untouched issue, want true")
	}

	cache.ClearProject("PVT_1")
	if _, ok := cache.GetItemID("PVT_1", "I_2"); ok {
		t.Fatal("GetItemID() ok = true after ClearProject(), want false")
	}
}

func TestProjectCacheReturnsFreshIssueRef(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.SetIssueRef("I_1", issueRef{Owner: "digitaldrywood", Name: "detent", Number: 313})

	got, ok := cache.GetIssueRef("I_1")
	if !ok {
		t.Fatal("GetIssueRef() ok = false, want true")
	}
	if got.Owner != "digitaldrywood" || got.Name != "detent" || got.Number != 313 {
		t.Fatalf("issue ref = %#v, want digitaldrywood/detent#313", got)
	}
}

func TestProjectCacheExpiresIssueRef(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.SetIssueRef("I_1", issueRef{Owner: "digitaldrywood", Name: "detent", Number: 313})

	now = now.Add(5*time.Minute - time.Nanosecond)
	if _, ok := cache.GetIssueRef("I_1"); !ok {
		t.Fatal("GetIssueRef() ok = false before TTL, want true")
	}

	now = now.Add(time.Nanosecond)
	if _, ok := cache.GetIssueRef("I_1"); ok {
		t.Fatal("GetIssueRef() ok = true after TTL, want false")
	}
}

func TestProjectCacheIgnoresBlankKeys(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.SetItemID("", "I_1", "PVTI_1")
	cache.SetItemID("PVT_1", "", "PVTI_1")
	cache.SetItemID("PVT_1", "I_1", "")

	if _, ok := cache.GetItemID("", "I_1"); ok {
		t.Fatal("GetItemID() ok = true for blank project, want false")
	}
	if _, ok := cache.GetItemID("PVT_1", ""); ok {
		t.Fatal("GetItemID() ok = true for blank issue, want false")
	}
	if _, ok := cache.GetItemID("PVT_1", "I_1"); ok {
		t.Fatal("GetItemID() ok = true for blank item, want false")
	}

	cache.SetIssueRef("", issueRef{Owner: "digitaldrywood", Name: "detent", Number: 313})
	cache.SetIssueRef("I_1", issueRef{Owner: "", Name: "detent", Number: 313})
	cache.SetIssueRef("I_1", issueRef{Owner: "digitaldrywood", Name: "", Number: 313})
	cache.SetIssueRef("I_1", issueRef{Owner: "digitaldrywood", Name: "detent"})
	if _, ok := cache.GetIssueRef("I_1"); ok {
		t.Fatal("GetIssueRef() ok = true for blank issue ref, want false")
	}
}

func TestProjectCacheTracksCompleteProjectFieldsScans(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		advance     time.Duration
		issueID     string
		wantPresent bool
		wantKnown   bool
	}{
		{name: "present", issueID: "I_1", wantPresent: true, wantKnown: true},
		{name: "negative membership", issueID: "I_missing", wantKnown: true},
		{name: "expired membership", advance: 5 * time.Minute, issueID: "I_1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := base
			cache := newProjectCache(5*time.Minute, func() time.Time { return now })
			updatedAt := base.Add(-time.Minute)
			cache.ReplaceProjectFields("PVT_1", map[string]projectItemFields{
				"I_1": {
					itemID:          "PVTI_1",
					statusName:      "In Progress",
					priorityName:    "P1",
					statusUpdatedAt: &updatedAt,
					fields:          map[string]string{"Owner": "worker-1"},
				},
			}, cache.Revision("PVT_1"))
			now = now.Add(test.advance)

			fields, present, known := cache.GetProjectFields("PVT_1", test.issueID)
			if present != test.wantPresent || known != test.wantKnown {
				t.Fatalf("GetProjectFields() = present %t known %t, want present %t known %t", present, known, test.wantPresent, test.wantKnown)
			}
			if !present {
				return
			}
			if fields.itemID != "PVTI_1" || fields.statusName != "In Progress" || fields.priorityName != "P1" {
				t.Fatalf("project fields = %#v, want cached item status and priority", fields)
			}
			fields.fields["Owner"] = "mutated"
			cached, _, _ := cache.GetProjectFields("PVT_1", "I_1")
			if cached.fields["Owner"] != "worker-1" {
				t.Fatalf("cached Owner = %q, want defensive copy", cached.fields["Owner"])
			}
		})
	}
}

func TestProjectCacheInvalidatesCompleteScanOnMembershipChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.ReplaceProjectFields("PVT_1", map[string]projectItemFields{
		"I_1": {itemID: "PVTI_1", statusName: "Todo"},
	}, cache.Revision("PVT_1"))

	cache.SetItemID("PVT_1", "I_2", "PVTI_2")
	if _, _, known := cache.GetProjectFields("PVT_1", "I_missing"); known {
		t.Fatal("GetProjectFields() known = true after SetItemID invalidated complete scan")
	}

	cache.ReplaceProjectFields("PVT_1", map[string]projectItemFields{
		"I_1": {itemID: "PVTI_1", statusName: "Todo"},
		"I_2": {itemID: "PVTI_2", statusName: "Todo"},
	}, cache.Revision("PVT_1"))
	cache.ClearItemID("PVT_1", "I_2")
	if _, _, known := cache.GetProjectFields("PVT_1", "I_missing"); known {
		t.Fatal("GetProjectFields() known = true after ClearItemID invalidated complete scan")
	}
}

func TestProjectCacheRejectsScanInvalidatedInFlight(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		invalidate func(*projectCache)
	}{
		{"fields invalidated", func(c *projectCache) { c.InvalidateProjectFields("PVT_1", "I_1") }},
		{"project cleared", func(c *projectCache) { c.ClearProject("PVT_1") }},
		{"item removed", func(c *projectCache) { c.ClearItemID("PVT_1", "I_1") }},
		{"item added", func(c *projectCache) { c.SetItemID("PVT_1", "I_2", "PVTI_2") }},
		{"targeted refresh", func(c *projectCache) {
			c.SetProjectFields("PVT_1", "I_2", projectItemFields{itemID: "PVTI_2", statusName: "Done"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			cache := newProjectCache(5*time.Minute, func() time.Time { return now })
			revision := cache.Revision("PVT_1")
			tt.invalidate(cache)
			cache.ReplaceProjectFields("PVT_1", map[string]projectItemFields{"I_1": {itemID: "PVTI_1", statusName: "Todo"}}, revision)
			if _, _, known := cache.GetProjectFields("PVT_1", "I_1"); known {
				t.Fatal("scan restored fields after invalidation")
			}
		})
	}
}

func TestProjectCacheInvalidatesOnlyChangedCard(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.ReplaceProjectFields("PVT_1", map[string]projectItemFields{
		"I_1": {itemID: "PVTI_1", statusName: "Todo"},
		"I_2": {itemID: "PVTI_2", statusName: "Todo"},
	}, cache.Revision("PVT_1"))
	cache.InvalidateProjectFields("PVT_1", "I_1")
	for _, tt := range []struct {
		id    string
		known bool
	}{{"I_1", false}, {"I_2", true}, {"I_missing", true}} {
		t.Run(tt.id, func(t *testing.T) {
			if _, _, known := cache.GetProjectFields("PVT_1", tt.id); known != tt.known {
				t.Fatalf("known = %t, want %t", known, tt.known)
			}
		})
	}
	if !cache.ProjectFieldsScanned("PVT_1") {
		t.Fatal("invalidation discarded complete scan")
	}
}
