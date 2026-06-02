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

func TestProjectCacheReturnsFreshItemStatus(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newProjectCache(5*time.Minute, func() time.Time { return now })
	cache.SetItem("PVT_1", "PR_1", projectItemStatus{ID: "PVTI_1", StatusName: "Merging"})

	got, ok := cache.GetItem("PVT_1", "PR_1")
	if !ok {
		t.Fatal("GetItem() ok = false, want true")
	}
	if got.ID != "PVTI_1" || got.StatusName != "Merging" {
		t.Fatalf("GetItem() = %#v, want item id PVTI_1 with Merging status", got)
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
}
