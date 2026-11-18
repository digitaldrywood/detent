package nativechange

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/policy"
)

func clientTestPolicy() policy.Descriptor {
	return hubtest.PolicyDescriptor()
}
