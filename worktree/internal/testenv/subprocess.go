package testenv

import "time"

// SubprocessWaitTimeout is a deadlock guard for condition-driven subprocess
// fixtures. It is intentionally generous so ordinary host scheduling delays do
// not become test behavior.
const SubprocessWaitTimeout = time.Minute
