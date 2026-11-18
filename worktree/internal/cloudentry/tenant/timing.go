package tenant

import (
	"errors"
	"time"
)

const CallTimeout = 3 * time.Second

var ErrMigrating = errors.New("tenant migrations are running")
