package api

import "time"

// realNow is the default value of timeNow used by handlers that need the
// current wall-clock. Pulled out of diagnostics.go so tests can re-bind
// timeNow to a fixed instant without touching handler code.
func realNow() time.Time { return time.Now().UTC() }
