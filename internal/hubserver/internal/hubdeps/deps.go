package hubdeps

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/update"
)

type Timer struct {
	C     <-chan time.Time
	Reset func(time.Duration)
	Stop  func()
}

type DashboardDelivery struct {
	NewTimer    func(time.Duration) Timer
	PageSize    int
	QueueSize   int
	TenantLimit int
	Budget      int
}

type Deps struct {
	RunMaintenance              func(context.Context, *hubmaintenance.Scheduler, func(error)) error
	Listen                      func(context.Context, string, string) (net.Listener, error)
	RetentionTimeout            func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	Jitter                      func() float64
	NewLeaseID                  func() string
	NewTokenID                  func() string
	GenerateToken               func() (string, error)
	NewOutboxTimer              func(time.Duration) Timer
	VerifyReleaseSignature      update.ChecksumSignatureVerifier
	DashboardDelivery           *DashboardDelivery
	ConversationStreamHeartbeat time.Duration
	ConversationStreamAuthorize time.Duration
}

func Default() Deps {
	return Deps{}.WithDefaults()
}

func (d Deps) WithDefaults() Deps {
	if d.RunMaintenance == nil {
		d.RunMaintenance = func(ctx context.Context, scheduler *hubmaintenance.Scheduler, report func(error)) error {
			return scheduler.Run(ctx, report)
		}
	}
	if d.Listen == nil {
		listenConfig := &net.ListenConfig{}
		d.Listen = listenConfig.Listen
	}
	if d.RetentionTimeout == nil {
		d.RetentionTimeout = context.WithTimeout
	}
	if d.Jitter == nil {
		d.Jitter = RandomJitter
	}
	if d.NewLeaseID == nil {
		d.NewLeaseID = uuid.NewString
	}
	if d.NewTokenID == nil {
		d.NewTokenID = uuid.NewString
	}
	if d.GenerateToken == nil {
		d.GenerateToken = apikey.GenerateToken
	}
	if d.NewOutboxTimer == nil {
		d.NewOutboxTimer = NewTimer
	}
	if d.VerifyReleaseSignature == nil {
		d.VerifyReleaseSignature = update.VerifyChecksumSignature
	}
	return d
}

func NewTimer(duration time.Duration) Timer {
	timer := time.NewTimer(duration)
	return Timer{C: timer.C, Reset: func(next time.Duration) { timer.Reset(next) }, Stop: func() { timer.Stop() }}
}

func RandomJitter() float64 {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0.5
	}
	return float64(binary.LittleEndian.Uint64(value[:])>>11) / (1 << 53)
}
