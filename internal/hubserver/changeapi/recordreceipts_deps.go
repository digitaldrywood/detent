package changeapi

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type RecordReceiptsDeps struct {
	Repository                   tenantstore.Repository
	Clock                        func() time.Time
	RunDetailStore               func() attachment.DetailStore
	AuthorizeAccessScope         func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error
	AuthorizeWorkItemRecordScope func(ctx context.Context, reader accessstore.Reader, scope hubscope.NativeScope) error
	HasAccessRepository          bool
}

type RecordReceipts struct {
	deps RecordReceiptsDeps
}

func NewRecordReceipts(deps RecordReceiptsDeps) *RecordReceipts {
	return &RecordReceipts{deps: deps}
}
