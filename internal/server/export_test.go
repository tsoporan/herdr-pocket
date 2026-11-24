package server

import "time"

// SetDenyWait shortens how long a deny with a note waits, for handlers made
// by New after the call. The returned func restores it.
func SetDenyWait(d time.Duration) (restore func()) {
	old := denyWait
	denyWait = d
	return func() { denyWait = old }
}
