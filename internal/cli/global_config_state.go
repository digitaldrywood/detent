package cli

import (
	"sync"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type globalConfigState struct {
	mu  sync.RWMutex
	cfg globalconfig.Config
}

func newGlobalConfigState(cfg globalconfig.Config) *globalConfigState {
	return &globalConfigState{cfg: cfg}
}

func (s *globalConfigState) get() globalconfig.Config {
	if s == nil {
		return globalconfig.Config{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *globalConfigState) set(cfg globalconfig.Config) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}
