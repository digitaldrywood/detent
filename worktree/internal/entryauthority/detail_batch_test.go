package entryauthority

import (
	"bytes"
	"context"
	"errors"
	"net/rpc"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type batchChannelDetails struct {
	channelDetailStore
	calls    atomic.Int64
	deadline time.Time
}

func (s *batchChannelDetails) ReadDetails(ctx context.Context, organization string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	s.calls.Add(1)
	s.deadline, _ = ctx.Deadline()
	rows := make([]attachment.DetailReadResult, len(refs))
	for index, ref := range refs {
		rows[index] = attachment.DetailReadResult{Reference: ref, Data: bytes.Clone(s.data)}
	}
	rows[len(rows)-1].ErrorCode = "missing"
	return rows, nil
}
func TestDetailBatchAuthorityBoundsScopeAndDeadline(t *testing.T) {
	binding := Binding{Issuer: "entry", Organization: "org_example", Generation: 2}
	store := &batchChannelDetails{}
	ref, err := store.StoreDetail(t.Context(), binding.Organization, []byte("example immutable content"))
	if err != nil {
		t.Fatal(err)
	}
	const budget = time.Second
	parent, err := Start(t.Context(), binding, budget, nil, Archives{Details: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	})
	client := &Client{client: rpc.NewClient(channel{reader: parent.Files[0], writer: parent.Files[1], budget: budget}), budget: budget, organization: binding.Organization}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	before := time.Now()
	rows, err := callAuthority[[]attachment.DetailReadResult](t.Context(), client, "ReadDetails", DetailBatchRequest{Organization: binding.Organization, References: []attachment.ArchiveReference{ref, ref}, Deadline: before.Add(time.Hour)})
	if err != nil || len(rows) != 2 {
		t.Fatalf("batch: %v", err)
	}
	if store.calls.Load() != 1 || store.deadline.After(time.Now().Add(budget)) || store.deadline.Before(before) {
		t.Fatal("authority budget or allocation count changed")
	}
	data, err := rows[0].Bytes()
	if err != nil || !bytes.Equal(data, store.data) {
		t.Fatalf("content: %v", err)
	}
	if _, err := rows[1].Bytes(); !errors.Is(err, artifact.ErrMissing) || len(rows[1].Data) != 0 {
		t.Fatalf("failed content retained: %v", err)
	}
	if _, err := callAuthority[[]attachment.DetailReadResult](t.Context(), client, "ReadDetails", DetailBatchRequest{Organization: "org_foreign", References: []attachment.ArchiveReference{ref}, Deadline: before.Add(time.Hour)}); err == nil {
		t.Fatal("foreign request accepted")
	}
	if store.calls.Load() != 1 {
		t.Fatal("foreign batch reached allocation owner")
	}
}
