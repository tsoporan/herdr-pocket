package herdr

import "time"

// SetRequestTimeout shortens the request timeout for one test.
func SetRequestTimeout(d time.Duration) (restore func()) {
	old := requestTimeout
	requestTimeout = d
	return func() { requestTimeout = old }
}
