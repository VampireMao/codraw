package api

import (
	"time"
)

// timeDuration is an alias so handler signatures stay readable.
type timeDuration = time.Duration

// secs converts seconds to a duration.
func secs(n int) time.Duration { return time.Duration(n) * time.Second }
