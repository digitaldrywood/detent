package scenariotest

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/policy"
)

func clientTestPolicy() policy.Descriptor {
	return hubtest.PolicyDescriptor()
}

func changeTestPolicy(t *testing.T, previous policy.Descriptor, edit func(*config.Workflow)) policy.Descriptor {
	t.Helper()
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, previous)
	if err != nil {
		t.Fatal(err)
	}
	edit(&workflow)
	descriptor, err := config.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}
