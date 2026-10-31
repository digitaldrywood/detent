package hubserver

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestSourceVerificationLifetimeAndFailureClass(t *testing.T) {
	for _, kind := range []string{"source", "landing"} {
		for _, outcome := range []string{"slow valid", "cancelled", "deadline", "infrastructure", "invalid"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
					defer cancel()
					infrastructure := errors.New("git unavailable")
					verify := func(ctx context.Context) error {
						switch outcome {
						case "cancelled":
							cancel()
						case "deadline":
							<-ctx.Done()
						case "infrastructure":
							return infrastructure
						case "invalid":
							return workspace.ErrInvalidVerification
						default:
							time.Sleep(16 * time.Second)
						}
						return ctx.Err()
					}
					s := &Service{sourceVerification: make(chan struct{}, 1),
						sourceVerifier:        func(ctx context.Context, _ tracker.ChangeSource, _ []byte) error { return verify(ctx) },
						incorporationVerifier: func(ctx context.Context, _ tracker.LandingIncorporation, _ []byte) error { return verify(ctx) },
					}
					var err error
					if kind == "source" {
						err = s.verifyChangeSource(ctx, tracker.ChangeSource{}, nil)
					} else {
						err = s.verifyLandingIncorporation(ctx, tracker.LandingIncorporation{}, nil)
					}
					switch outcome {
					case "slow valid":
						if err != nil {
							t.Fatal(err)
						}
					case "cancelled":
						if !errors.Is(err, context.Canceled) {
							t.Fatal(err)
						}
					case "deadline":
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatal(err)
						}
					case "infrastructure":
						if !errors.Is(err, infrastructure) {
							t.Fatal(err)
						}
					case "invalid":
						var invalid *nativeError
						if !errors.As(err, &invalid) || invalid.status != 422 {
							t.Fatalf("invalid proof = %v", err)
						}
					}
				})
			})
		}
	}
}
