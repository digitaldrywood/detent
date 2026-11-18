package remoterunner

import (
	"context"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/project"
)

func WaitForUpdateDrain(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func UpdateDrainTimeout(registry *project.Registry) time.Duration {
	defaultTimeout := max(
		time.Duration(workflowconfig.DefaultMaxSessionDurationMS)*time.Millisecond,
		time.Duration(workflowconfig.DefaultMergeWorkerMaxDurationMS)*time.Millisecond,
	)
	if registry == nil {
		return defaultTimeout
	}
	timeout := time.Duration(0)
	for _, trackedProject := range registry.List() {
		agent := trackedProject.Workflow().Config.Agent
		for _, level := range trackedProject.Workflow().Config.Agents.ModelSelection.Levels {
			if level.MaxSessionDurationMS != nil {
				timeout = max(timeout, time.Duration(*level.MaxSessionDurationMS)*time.Millisecond)
			}
		}
		ceilings := []struct {
			configured int
			fallback   int
		}{
			{configured: agent.MaxSessionDurationMS, fallback: workflowconfig.DefaultMaxSessionDurationMS},
			{configured: agent.MergeWorkerMaxDurationMS, fallback: workflowconfig.DefaultMergeWorkerMaxDurationMS},
		}
		for _, ceiling := range ceilings {
			configured := ceiling.configured
			if configured <= 0 {
				configured = ceiling.fallback
			}
			timeout = max(timeout, time.Duration(configured)*time.Millisecond)
		}
	}
	if timeout == 0 {
		return defaultTimeout
	}
	return timeout
}
