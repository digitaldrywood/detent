package cloudassert

import (
	"strconv"
	"testing"
	"time"
)

func TestReplayCapacityIsSharedAcrossOrganizations(t *testing.T) {
	var cache ReplayCache
	now := time.Unix(100, 0)
	for i := 0; i < maxReplayCache; i++ {
		if !cache.remember("org_alpha", strconv.Itoa(i), now.Add(MaxLifetime), now, maxReplayCache/2) {
			break
		}
	}
	if !cache.remember("org_beta", "0", now.Add(MaxLifetime), now, maxReplayCache/2) {
		t.Fatal("saturated tenant denied another tenant replay capacity")
	}
	if cache.remember("org_alpha", "0", now.Add(MaxLifetime), now, maxReplayCache/2) {
		t.Fatal("active replay record evicted")
	}
	later := now.Add(MaxLifetime + clockSkew)
	if !cache.remember("org_alpha", "0", later.Add(MaxLifetime), later, maxReplayCache/2) {
		t.Fatal("expired replay capacity not recovered")
	}
}
