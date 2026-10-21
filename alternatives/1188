package github

import (
	"testing"
	"time"
)

func TestStatusCacheReturnsFreshMetadataByProject(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newStatusCache(5*time.Minute, func() time.Time { return now })

	cache.Set("PVT_1", statusMetadata{
		FieldID: "PVTSSF_status",
		OptionIDsByName: map[string]string{
			"Done": "OPT_done",
			"Todo": "OPT_todo",
		},
	})
	cache.Set("PVT_2", statusMetadata{
		FieldID:         "PVTSSF_other",
		OptionIDsByName: map[string]string{"Done": "OPT_other"},
	})

	got, ok := cache.Get("PVT_1")
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	if got.FieldID != "PVTSSF_status" {
		t.Fatalf("FieldID = %q, want PVTSSF_status", got.FieldID)
	}
	if got.OptionIDsByName["Done"] != "OPT_done" {
		t.Fatalf("Done option = %q, want OPT_done", got.OptionIDsByName["Done"])
	}
}

func TestStatusCacheExpiresMetadata(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newStatusCache(5*time.Minute, func() time.Time { return now })
	cache.Set("PVT_1", statusMetadata{FieldID: "PVTSSF_status"})

	now = now.Add(5*time.Minute - time.Nanosecond)
	if _, ok := cache.Get("PVT_1"); !ok {
		t.Fatal("Get() ok = false before TTL, want true")
	}

	now = now.Add(time.Nanosecond)
	if _, ok := cache.Get("PVT_1"); ok {
		t.Fatal("Get() ok = true after TTL, want false")
	}
}

func TestStatusCacheClonesOptionIDs(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newStatusCache(5*time.Minute, func() time.Time { return now })
	options := map[string]string{"Done": "OPT_done"}
	cache.Set("PVT_1", statusMetadata{
		FieldID:         "PVTSSF_status",
		OptionIDsByName: options,
	})

	options["Done"] = "OPT_mutated"
	got, ok := cache.Get("PVT_1")
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	if got.OptionIDsByName["Done"] != "OPT_done" {
		t.Fatalf("Done option = %q, want OPT_done", got.OptionIDsByName["Done"])
	}

	got.OptionIDsByName["Done"] = "OPT_changed"
	got, ok = cache.Get("PVT_1")
	if !ok {
		t.Fatal("Get() ok = false after caller mutation, want true")
	}
	if got.OptionIDsByName["Done"] != "OPT_done" {
		t.Fatalf("cached Done option = %q, want OPT_done", got.OptionIDsByName["Done"])
	}
}

func TestStatusCacheClearProject(t *testing.T) {
	t.Parallel()

	now := time.Now()
	cache := newStatusCache(5*time.Minute, func() time.Time { return now })
	cache.Set("PVT_1", statusMetadata{FieldID: "PVTSSF_status"})
	cache.Clear("PVT_1")

	if _, ok := cache.Get("PVT_1"); ok {
		t.Fatal("Get() ok = true after Clear(), want false")
	}
}
