package hookproc

import (
	"fmt"
	"syscall"

	"github.com/digitaldrywood/detent/internal/procgroup/darwinproc"
)

type auditToken = darwinproc.AuditToken

type hookProcessInstances struct {
	capture func(int) (auditToken, error)
	tokens  map[int]auditToken
}

func platformHookSignals(lifetime *Lifetime) hookSignals {
	instances := new(hookProcessInstances)
	if lifetime != nil {
		instances = &lifetime.instances
	}
	if instances.capture == nil {
		instances.capture = darwinproc.Capture
	}
	return instances
}

func (instances *hookProcessInstances) prepare(pid int) (func(), error) {
	if _, exists := instances.tokens[pid]; exists {
		return nil, nil
	}
	token, err := instances.capture(pid)
	if err != nil {
		return nil, err
	}
	return func() {
		if instances.tokens == nil {
			instances.tokens = make(map[int]auditToken)
		}
		instances.tokens[pid] = token
	}, nil
}

func (instances *hookProcessInstances) signal(pid int, sig syscall.Signal) error {
	token, ok := instances.tokens[pid]
	if !ok {
		return fmt.Errorf("missing verified hook audit token for PID %d", pid)
	}
	return token.Signal(sig)
}
