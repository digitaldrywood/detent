package coordination

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestPeerLaneWriteAcknowledgement(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		instance string
		project  string
		issue    string
		to       string
		observed time.Time
		want     bool
	}{
		{name: "peer", instance: "local", project: "project", issue: "issue", to: "Todo", observed: at.Add(time.Second), want: true},
		{name: "local identity", instance: "peer", project: "project", issue: "issue", to: "Todo", observed: at.Add(time.Second)},
		{name: "other project", instance: "local", project: "other", issue: "issue", to: "Todo", observed: at.Add(time.Second)},
		{name: "other issue", instance: "local", project: "project", issue: "other", to: "Todo", observed: at.Add(time.Second)},
		{name: "other lane", instance: "local", project: "project", issue: "issue", to: "Done", observed: at.Add(time.Second)},
		{name: "future write", instance: "local", project: "project", issue: "issue", to: "Todo", observed: at.Add(-time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := &laneTestStore{records: make(map[string]Record)}
			write := LaneWrite{InstanceIdentity: "peer", Issue: "issue", From: "Backlog", To: "Todo", Reason: "admission", FenceToken: 1, WrittenAt: at}
			if err := PublishLaneWrite(t.Context(), backend, "project", write); err != nil {
				t.Fatal(err)
			}
			got, err := MatchPeerLaneWrite(t.Context(), backend, tt.project, tt.instance, tt.issue, tt.to, time.Time{}, tt.observed)
			if err != nil || got != tt.want {
				t.Fatalf("MatchPeerLaneWrite() = %t, %v; want %t", got, err, tt.want)
			}
		})
	}
}

func TestLaneWriteFencing(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, token := range []uint64{1, 2, 3} {
		t.Run(strconv.FormatUint(token, 10), func(t *testing.T) {
			t.Parallel()
			backend := &laneTestStore{records: make(map[string]Record)}
			write := LaneWrite{InstanceIdentity: "peer", Issue: "issue", From: "Todo", To: "In Progress", Reason: "dispatch", FenceToken: 2, WrittenAt: at}
			if err := PublishLaneWrite(t.Context(), backend, "project", write); err != nil {
				t.Fatal(err)
			}
			if err := PublishLaneWrite(t.Context(), backend, "project", write); err != nil {
				t.Fatalf("idempotent publication: %v", err)
			}
			write.To = "Done"
			write.FenceToken = token
			err := PublishLaneWrite(t.Context(), backend, "project", write)
			if (err != nil) != (token <= 2) {
				t.Fatalf("PublishLaneWrite() = %v, token %d", err, token)
			}
			matched, err := MatchPeerLaneWrite(t.Context(), backend, "project", "local", "issue", "In Progress", time.Time{}, at.Add(time.Second))
			if err != nil || matched != (token <= 2) {
				t.Fatalf("superseded acknowledgement = %t, %v", matched, err)
			}
		})
	}
}

func TestLaneWriteBackendAvailability(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		backend Store
		wantErr bool
	}{
		{name: "disabled"},
		{name: "unavailable", backend: &laneTestStore{err: errors.New("unavailable")}, wantErr: true},
		{name: "malformed", backend: &laneTestStore{records: map[string]Record{laneWriteKey("project", "issue"): {Value: []byte("invalid")}}}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			matched, err := MatchPeerLaneWrite(t.Context(), tt.backend, "project", "local", "issue", "Todo", time.Time{}, time.Now())
			if matched || (err != nil) != tt.wantErr {
				t.Fatalf("MatchPeerLaneWrite() = %t, %v", matched, err)
			}
		})
	}
}

type laneTestStore struct {
	mu      sync.Mutex
	records map[string]Record
	err     error
	version int
}

func (s *laneTestStore) Get(_ context.Context, key string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found := s.records[key]
	return record, found, s.err
}

func (s *laneTestStore) CompareAndSwap(_ context.Context, key, version string, value []byte) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return Record{}, false, s.err
	}
	if s.records[key].Version != version {
		return s.records[key], false, nil
	}
	s.version++
	record := Record{Value: append([]byte(nil), value...), Version: strconv.Itoa(s.version)}
	s.records[key] = record
	return record, true, nil
}
