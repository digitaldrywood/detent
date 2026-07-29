package linear

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorDeclinesUnsupportedCandidateSelector(t *testing.T) {
	t.Parallel()

	c := &Connector{}
	if c.CandidateCapabilities().Supports(connector.CandidateSelectorStates) {
		t.Fatal("CandidateCapabilities().Supports(states) = true, want false")
	}
	_, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Backlog"},
		Limit:    10,
	})
	if !errors.Is(err, connector.ErrCandidateSelectorUnsupported) {
		t.Fatalf("ReadCandidates() error = %v, want ErrCandidateSelectorUnsupported", err)
	}
}
