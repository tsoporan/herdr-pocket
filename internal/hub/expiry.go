package hub

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// Expiry is the tailnet sign-in's end, sent only when it is near (SPEC UX 8).
type Expiry struct {
	At time.Time `json:"at"`
	// DaysLeft is whole days, rounded up: 1 is "ends tomorrow", 0 or less
	// "has ended".
	DaysLeft int `json:"days_left"`
}

// ExpiryWarnDays is how near the end has to be before the app warns.
const ExpiryWarnDays = 14

// expiryEvery is how often days_left is recomputed.
const expiryEvery = time.Hour

// DaysLeft is the whole days until t, rounded up, as the app shows them:
// 1 is "ends tomorrow", 0 or less "has ended".
func DaysLeft(t, now time.Time) int { return int(math.Ceil(t.Sub(now).Hours() / 24)) }

// ExpiryAt is what the app is told about a node key that expires at
// keyExpiry: nil when expiry is off or more than ExpiryWarnDays away.
func ExpiryAt(keyExpiry *time.Time, now time.Time) *Expiry {
	if keyExpiry == nil {
		return nil
	}
	days := DaysLeft(*keyExpiry, now)
	if days > ExpiryWarnDays {
		return nil
	}
	return &Expiry{At: keyExpiry.UTC().Truncate(time.Second), DaysLeft: days}
}

// ExpiryPatch is a patch's expiry field. A patch without one leaves the
// expiry unchanged; a nil Value is sent as null and clears the banner.
type ExpiryPatch struct{ Value *Expiry }

func (p ExpiryPatch) MarshalJSON() ([]byte, error) { return json.Marshal(p.Value) }

func (p *ExpiryPatch) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &p.Value) }

// SetKeyExpiry records the node key's expiry (nil when expiry is off) and
// sends subscribers a patch if what the app shows changes.
func (h *Hub) SetKeyExpiry(t *time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keyExpiry = t
	if h.pusher != nil {
		h.pusher.SetExpiry(t)
	}
	h.refreshExpiryLocked()
}

// refreshExpiryLocked recomputes the expiry and patches subscribers when it
// changed. Callers hold h.mu.
func (h *Hub) refreshExpiryLocked() {
	e := ExpiryAt(h.keyExpiry, h.now())
	if equalExpiry(e, h.expiry) {
		return
	}
	h.expiry = e
	p := Patch{Type: "patch", Expiry: &ExpiryPatch{Value: e}}
	for s := range h.subs {
		h.sendLocked(s, p)
	}
}

func equalExpiry(a, b *Expiry) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.At.Equal(b.At) && a.DaysLeft == b.DaysLeft
}

// expiryLoop recomputes days_left hourly, so the banner appears and counts
// down without a new key expiry.
func (h *Hub) expiryLoop(ctx context.Context) {
	t := time.NewTicker(expiryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.mu.Lock()
			h.refreshExpiryLocked()
			h.mu.Unlock()
		}
	}
}
