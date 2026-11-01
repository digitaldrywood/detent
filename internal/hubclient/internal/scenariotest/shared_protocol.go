package scenariotest

import "github.com/digitaldrywood/detent/internal/hubclient/api"

func nativeLeaseLost(err error) bool { return api.NativeLeaseLost(err) }

func executionID(prefix, value string) string { return api.ExecutionID(prefix, value) }

func nativeTransportUnavailable(err error) bool { return api.NativeTransportUnavailable(err) }
