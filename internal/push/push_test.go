package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const owner = "owner@example.com"

func blocked(seq uint64) Agent {
	return Agent{PaneID: "w1-p2", Name: "api tests", Workspace: "orchard", Status: "blocked", Seq: seq}
}

func with(a Agent, status string, seq uint64) Agent {
	a.Status, a.Seq = status, seq
	return a
}

func noDesk() Settings { s := DefaultSettings(); s.DeskMinutes = 0; return s }

func TestBlockedAlertIsEncryptedWithHeaders(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, DefaultSettings())
	r.svc.Observe([]Agent{with(blocked(1), "working", 1)}) // baseline
	r.svc.Observe([]Agent{blocked(2)})

	if got := r.at(2 * time.Second); len(got) != 0 {
		t.Fatalf("sent before settle: %d", len(got))
	}
	got := r.decode(d, r.at(3*time.Second))
	if len(got) != 1 {
		t.Fatalf("got %d pushes, want 1", len(got))
	}
	want := message{Kind: "blocked", Tag: "agent-w1-p2", Title: "api tests", Body: "Asks you a question · orchard. Tap to answer.", URL: "/#agent=w1-p2"}
	if got[0].msg != want {
		t.Fatalf("payload %+v, want %+v", got[0].msg, want)
	}
	if got[0].urgency != "high" {
		t.Fatalf("Urgency %q", got[0].urgency)
	}
	if len(got[0].topic) != 32 {
		t.Fatalf("Topic length %d", len(got[0].topic))
	}
	// Still blocked with the same seq: no repeat after the cooldown.
	if got := r.at(5 * time.Minute); len(got) != 0 {
		t.Fatalf("repeat alert: %d", len(got))
	}
}

func TestSettleSkipsFlipBack(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk())
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	r.clk.Set(r.start.Add(time.Second))
	r.svc.Observe([]Agent{with(blocked(0), "working", 3)})
	if got := r.at(10 * time.Second); len(got) != 0 {
		t.Fatalf("a state that flipped back was sent: %d", len(got))
	}
	// A new seq restarts the settle delay.
	r.clk.Set(r.start.Add(20 * time.Second))
	r.svc.Observe([]Agent{blocked(4)})
	r.clk.Set(r.start.Add(22 * time.Second))
	r.svc.Observe([]Agent{blocked(5)})
	if got := r.at(24 * time.Second); len(got) != 0 {
		t.Fatalf("sent before the new seq settled: %d", len(got))
	}
	got := r.decode(d, r.at(25*time.Second))
	if len(got) != 1 || got[0].msg.Kind != "blocked" {
		t.Fatalf("got %+v", got)
	}
}

func TestBaselineAndNewSubscriptionDoNotAlertExistingState(t *testing.T) {
	r := newRig(t)
	a := newDevice(t, "dev-a")
	r.subscribe(a, noDesk())
	r.svc.Observe([]Agent{blocked(2)}) // already blocked when the server starts
	b := newDevice(t, "dev-b")
	r.subscribe(b, noDesk())
	if got := r.at(time.Minute); len(got) != 0 {
		t.Fatalf("existing state alerted: %d", len(got))
	}
}

func TestCooldownHoldsThenSends(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk())
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	if got := r.at(3 * time.Second); len(got) != 1 {
		t.Fatalf("first alert: %d", len(got))
	}
	r.clk.Set(r.start.Add(10 * time.Second))
	r.svc.Observe([]Agent{with(blocked(0), "working", 3)})
	if got := r.decode(d, r.at(10*time.Second)); len(got) != 1 || got[0].msg.Kind != "clear" {
		t.Fatalf("want one clear, got %+v", got)
	}
	r.clk.Set(r.start.Add(20 * time.Second))
	r.svc.Observe([]Agent{blocked(4)})
	if got := r.at(30 * time.Second); len(got) != 0 {
		t.Fatalf("sent inside the cooldown: %d", len(got))
	}
	if got := r.at(62 * time.Second); len(got) != 0 {
		t.Fatalf("sent before the cooldown ended: %d", len(got))
	}
	got := r.decode(d, r.at(63*time.Second))
	if len(got) != 1 || got[0].msg.Kind != "blocked" {
		t.Fatalf("held alert not sent when the cooldown ended: %+v", got)
	}
}

func TestHeldAlertDroppedWhenStateChanged(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, DefaultSettings()) // 5 desk minutes
	focused := blocked(2)
	focused.Focused = true
	r.svc.Observe(nil)
	r.svc.FocusEvent(r.start)
	r.svc.Observe([]Agent{focused})
	if got := r.at(3 * time.Second); len(got) != 0 {
		t.Fatalf("sent while at the desk: %d", len(got))
	}
	r.clk.Set(r.start.Add(2 * time.Minute))
	r.svc.Observe([]Agent{with(focused, "working", 3)}) // answered at the desk
	if got := r.at(6 * time.Minute); len(got) != 0 {
		t.Fatalf("a stale held alert was sent: %d", len(got))
	}
}

func TestDeskSuppressionHoldsUntilWindowEnds(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	off := newDevice(t, "dev-off")
	r.subscribe(d, DefaultSettings())
	r.subscribe(off, noDesk())
	focused := blocked(2)
	focused.Focused = true
	r.svc.Observe(nil)
	// Focus the pane, walk away; it blocks a minute later.
	r.svc.FocusEvent(r.start)
	r.clk.Set(r.start.Add(time.Minute))
	r.svc.Observe([]Agent{focused})
	got := r.at(time.Minute + 3*time.Second)
	if len(got) != 1 || got[0].path != endpointPath(off) {
		t.Fatalf("want only the desk-off device, got %d", len(got))
	}
	if got := r.at(5*time.Minute - time.Second); len(got) != 0 {
		t.Fatalf("sent inside the desk window: %d", len(got))
	}
	if got := r.decode(d, r.at(5*time.Minute)); len(got) != 1 || got[0].msg.Kind != "blocked" {
		t.Fatalf("held alert not sent when the desk window ended: %+v", got)
	}
}

func TestDeskNeedsFocusedAgent(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, DefaultSettings())
	r.svc.Observe(nil)
	r.svc.FocusEvent(r.start)
	r.svc.Observe([]Agent{blocked(2)}) // another pane is focused
	if got := r.at(3 * time.Second); len(got) != 1 {
		t.Fatalf("an unfocused agent was suppressed: %d", len(got))
	}
}

func TestQuietHoursAcrossMidnightInDeviceZone(t *testing.T) {
	r := newRig(t)
	// 03:00 UTC is 22:00 in Toronto (EST) and 12:00 in Tokyo.
	r.start = time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	r.clk.Set(r.start)
	quiet := func(tz string) Settings {
		s := noDesk()
		s.Quiet = &Quiet{Start: "22:00", End: "07:00", TZ: tz}
		return s
	}
	toronto, tokyo := newDevice(t, "dev-toronto"), newDevice(t, "dev-tokyo")
	r.subscribe(toronto, quiet("America/Toronto"))
	r.subscribe(tokyo, quiet("Asia/Tokyo"))
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	got := r.at(3 * time.Second)
	if len(got) != 1 || got[0].path != endpointPath(tokyo) {
		t.Fatalf("want only Tokyo (outside its window), got %d", len(got))
	}
	// 07:00 Toronto is 12:00 UTC, nine hours on.
	if got := r.at(9*time.Hour - time.Second); len(got) != 0 {
		t.Fatalf("sent inside quiet hours: %d", len(got))
	}
	if got := r.decode(toronto, r.at(9*time.Hour)); len(got) != 1 || got[0].msg.Kind != "blocked" {
		t.Fatalf("held alert not sent when quiet hours ended: %+v", got)
	}
}

func TestQuietWindow(t *testing.T) {
	utc := func(h, m int) time.Time { return time.Date(2026, 11, 22, h, m, 0, 0, time.UTC) }
	cases := []struct {
		start, end string
		now        time.Time
		inside     bool
		ends       time.Time
	}{
		{"09:00", "17:00", utc(8, 59), false, time.Time{}},
		{"09:00", "17:00", utc(9, 0), true, utc(17, 0)},
		{"09:00", "17:00", utc(17, 0), false, time.Time{}},
		{"22:00", "07:00", utc(23, 30), true, utc(7, 0).Add(24 * time.Hour)},
		{"22:00", "07:00", utc(6, 59), true, utc(7, 0)},
		{"22:00", "07:00", utc(7, 0), false, time.Time{}},
		{"08:00", "08:00", utc(8, 0), false, time.Time{}},
	}
	// A zone's own wall clock decides, across both DST changes.
	toronto := []struct {
		start, end string
		now, ends  time.Time
	}{
		// 2026-11-01 falls back 02:00 EDT -> 01:00 EST. 06:10 UTC is the
		// second 01:10, and the window ends at the next 01:30 (EST).
		{"23:00", "01:30", time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC), time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)},
		// The first 01:10 (EDT) ends at the first 01:30.
		{"23:00", "01:30", time.Date(2026, 11, 1, 5, 10, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		// 2026-03-08 springs forward 02:00 EST -> 03:00 EDT: from 01:50 EST
		// a 03:30 end is 40 minutes away, not 100.
		{"23:00", "03:30", time.Date(2026, 3, 8, 6, 50, 0, 0, time.UTC), time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC)},
	}
	for _, c := range toronto {
		w, err := parseWindow(Quiet{Start: c.start, End: c.end, TZ: "America/Toronto"})
		if err != nil {
			t.Fatal(err)
		}
		ends, inside := w.endIfInside(c.now)
		if !inside || !ends.Equal(c.ends) {
			t.Errorf("Toronto %s-%s at %s: inside=%v ends=%s, want %s", c.start, c.end, c.now, inside, ends.UTC(), c.ends)
		}
	}
	for _, c := range cases {
		w, err := parseWindow(Quiet{Start: c.start, End: c.end, TZ: "UTC"})
		if err != nil {
			t.Fatal(err)
		}
		ends, inside := w.endIfInside(c.now)
		if inside != c.inside || !ends.Equal(c.ends) {
			t.Errorf("%s-%s at %s: inside=%v ends=%s, want %v %s", c.start, c.end, c.now.Format("15:04"), inside, ends, c.inside, c.ends)
		}
	}
}

func TestClearOnlyToDevicesThatGotTheAlert(t *testing.T) {
	r := newRig(t)
	got, skipped, failed := newDevice(t, "dev-got"), newDevice(t, "dev-skipped"), newDevice(t, "dev-failed")
	r.subscribe(got, noDesk())
	none := noDesk()
	none.Blocked = false
	r.subscribe(skipped, none)
	r.subscribe(failed, noDesk())
	r.ps.setStatus(failed, http.StatusInternalServerError)
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	if n := len(r.at(3 * time.Second)); n != 2 {
		t.Fatalf("alerts: %d, want 2 (got and failed)", n)
	}
	r.clk.Set(r.start.Add(10 * time.Second))
	r.svc.Observe([]Agent{with(blocked(0), "working", 3)})
	clears := r.decode(got, r.at(10*time.Second))
	if len(clears) != 1 {
		t.Fatalf("clears: %d, want 1", len(clears))
	}
	want := message{Kind: "clear", Tag: "agent-w1-p2", Title: "api tests", URL: "/#agent=w1-p2"}
	if clears[0].msg != want || clears[0].urgency != "low" {
		t.Fatalf("clear %+v urgency %q", clears[0].msg, clears[0].urgency)
	}
	// Leaving blocked again sends nothing: the alert is no longer shown.
	r.svc.Observe([]Agent{with(blocked(0), "idle", 4)})
	if got := r.at(20 * time.Second); len(got) != 0 {
		t.Fatalf("second clear: %d", len(got))
	}
}

func TestClearWhenPaneVanishes(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk())
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	r.at(3 * time.Second)
	r.svc.Observe(nil) // the pane closed
	got := r.decode(d, r.at(4*time.Second))
	if len(got) != 1 || got[0].msg.Kind != "clear" || got[0].msg.Tag != "agent-w1-p2" {
		t.Fatalf("got %+v", got)
	}
	if len(r.svc.slots) != 0 {
		t.Fatalf("slots left for a vanished pane: %d", len(r.svc.slots))
	}
}

func TestDoneOnlyWhenEnabled(t *testing.T) {
	r := newRig(t)
	on, off := newDevice(t, "dev-on"), newDevice(t, "dev-off")
	set := noDesk()
	set.Done = true
	r.subscribe(on, set)
	r.subscribe(off, noDesk())
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{with(blocked(0), "done", 2)})
	got := r.decode(on, r.at(3*time.Second))
	if len(got) != 1 {
		t.Fatalf("done pushes: %d, want 1", len(got))
	}
	want := message{Kind: "done", Tag: "agent-w1-p2", Title: "api tests", Body: "Finished · orchard", URL: "/#agent=w1-p2"}
	if got[0].msg != want || got[0].urgency != "normal" {
		t.Fatalf("done %+v urgency %q", got[0].msg, got[0].urgency)
	}
}

func TestEnablingTriggerDoesNotAlertOldState(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk()) // done off
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{with(blocked(0), "done", 2)})
	if got := r.at(3 * time.Second); len(got) != 0 {
		t.Fatalf("done sent with done off: %d", len(got))
	}
	set := noDesk()
	set.Done = true
	r.subscribe(d, set)
	if got := r.at(10 * time.Second); len(got) != 0 {
		t.Fatalf("turning done on alerted an old state: %d", len(got))
	}
	r.svc.Observe([]Agent{with(blocked(0), "done", 4)})
	if got := r.at(14 * time.Second); len(got) != 1 {
		t.Fatalf("a new done after turning it on: %d, want 1", len(got))
	}
}

func TestExpiryOncePerValue(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk())
	expiry := r.start.Add(8 * 24 * time.Hour)
	r.svc.SetExpiry(&expiry)
	if got := r.at(0); len(got) != 0 {
		t.Fatalf("sent at 8 days: %d", len(got))
	}
	got := r.decode(d, r.at(24*time.Hour))
	if len(got) != 1 || got[0].msg.Kind != "expiry" || got[0].msg.Title != "Tailscale sign-in ends in 7 days" || got[0].msg.URL != "/" || got[0].urgency != "normal" {
		t.Fatalf("7-day push: %+v", got)
	}
	if got := r.at(25 * time.Hour); len(got) != 0 {
		t.Fatalf("7-day push repeated: %d", len(got))
	}
	// A restart keeps the mark.
	r.svc = r.open()
	r.svc.SetExpiry(&expiry)
	if got := r.at(26 * time.Hour); len(got) != 0 {
		t.Fatalf("7-day push repeated after a restart: %d", len(got))
	}
	got = r.decode(d, r.at(7*24*time.Hour))
	if len(got) != 1 || got[0].msg.Title != "Tailscale sign-in ends tomorrow" {
		t.Fatalf("1-day push: %+v", got)
	}
	if got := r.at(7*24*time.Hour + time.Hour); len(got) != 0 {
		t.Fatalf("1-day push repeated: %d", len(got))
	}
	// Signing in again gives a new expiry value; its pushes start over.
	renewed := r.start.Add(10 * 24 * time.Hour)
	r.svc.SetExpiry(&renewed)
	if got := r.decode(d, r.at(7*24*time.Hour+2*time.Hour)); len(got) != 1 || got[0].msg.Title != "Tailscale sign-in ends in 3 days" {
		t.Fatalf("push for the new expiry: %+v", got)
	}
	r.svc.SetExpiry(nil)
	if got := r.at(9*24*time.Hour + 12*time.Hour); len(got) != 0 {
		t.Fatalf("pushed with expiry off: %d", len(got))
	}
}

func TestExpiryWaitsForQuietHours(t *testing.T) {
	r := newRig(t) // 15:00 UTC
	d := newDevice(t, "dev-a")
	set := noDesk()
	set.Quiet = &Quiet{Start: "14:00", End: "16:00", TZ: "UTC"}
	r.subscribe(d, set)
	expiry := r.start.Add(3 * 24 * time.Hour)
	r.svc.SetExpiry(&expiry)
	if got := r.at(0); len(got) != 0 {
		t.Fatalf("expiry push inside quiet hours: %d", len(got))
	}
	if got := r.at(time.Hour); len(got) != 1 {
		t.Fatalf("expiry push after quiet hours: %d", len(got))
	}
}

func TestGoneSubscriptionIsDeleted(t *testing.T) {
	for _, code := range []int{http.StatusGone, http.StatusNotFound} {
		r := newRig(t)
		d := newDevice(t, "dev-a")
		r.subscribe(d, noDesk())
		r.ps.setStatus(d, code)
		r.svc.Observe(nil)
		r.svc.Observe([]Agent{blocked(2)})
		if n := len(r.at(3 * time.Second)); n != 1 {
			t.Fatalf("%d: pushes %d", code, n)
		}
		if _, err := r.svc.Get(owner, d.sub.Endpoint); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%d: Get after gone: %v", code, err)
		}
		b, err := os.ReadFile(filepath.Join(r.dir, "push", "subscriptions.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), d.sub.Endpoint) {
			t.Fatalf("%d: subscriptions.json still has the endpoint", code)
		}
		if logs := r.logs.String(); !strings.Contains(logs, "fcm.googleapis.com") || strings.Contains(logs, endpointPath(d)) {
			t.Fatalf("%d: logs must name the host only: %q", code, logs)
		}
	}
}

func TestFailureLogsHostOnly(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-secret-token")
	r.subscribe(d, noDesk())
	r.ps.setStatus(d, http.StatusInternalServerError)
	err := r.svc.Test(r.ctx, owner, d.sub.Endpoint)
	if !errors.Is(err, ErrPushFailed) {
		t.Fatalf("Test: %v", err)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "fcm.googleapis.com") || strings.Contains(logs, "dev-secret-token") || strings.Contains(err.Error(), "dev-secret-token") {
		t.Fatalf("endpoint path leaked: %q / %v", logs, err)
	}
	// A transport error (*url.Error) must not leak it either.
	r.ps.srv.Close()
	err = r.svc.Test(r.ctx, owner, d.sub.Endpoint)
	if !errors.Is(err, ErrPushFailed) || strings.Contains(err.Error(), "dev-secret-token") || strings.Contains(r.logs.String(), "dev-secret-token") {
		t.Fatalf("transport error leaked the path: %v / %q", err, r.logs.String())
	}
}

func TestTestPushIgnoresQuietAndDesk(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	set := DefaultSettings()
	set.Quiet = &Quiet{Start: "00:00", End: "23:59", TZ: "UTC"}
	r.subscribe(d, set)
	r.svc.FocusEvent(r.start)
	if err := r.svc.Test(r.ctx, owner, d.sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	got := r.decode(d, r.ps.take())
	if len(got) != 1 || got[0].msg.Kind != "test" || got[0].msg.URL != "/" || got[0].urgency != "normal" {
		t.Fatalf("test push %+v", got)
	}
	if err := r.svc.Test(r.ctx, "other@example.com", d.sub.Endpoint); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another login's test: %v", err)
	}
}

func TestOnlyOwnerChangesSubscription(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, DefaultSettings())
	other := "other@example.com"
	if _, err := r.svc.Subscribe(other, "laptop", d.sub, DefaultSettings()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Subscribe by another login: %v", err)
	}
	if _, err := r.svc.Get(other, d.sub.Endpoint); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Get by another login: %v", err)
	}
	if err := r.svc.Unsubscribe(other, d.sub.Endpoint); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Unsubscribe by another login: %v", err)
	}
	set := DefaultSettings()
	set.Done, set.DeskMinutes = true, 10
	if stored, err := r.svc.Subscribe(owner, "phone", d.sub, set); err != nil || stored != set {
		t.Fatalf("owner upsert: %+v %v", stored, err)
	}
	if got, err := r.svc.Get(owner, d.sub.Endpoint); err != nil || got != set {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if err := r.svc.Unsubscribe(owner, d.sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Get(owner, d.sub.Endpoint); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after unsubscribe: %v", err)
	}
	if err := r.svc.Unsubscribe(owner, d.sub.Endpoint); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unsubscribe: %v", err)
	}
}

func TestSubscribeRefusesBadEndpoints(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	for _, ep := range []string{
		"http://fcm.googleapis.com/fcm/send/x",
		"https://example.com/push",
		"https://142.250.1.1/fcm/send/x",
		"https://[2607:f8b0::1]/fcm/send/x",
		"https://127.0.0.1/fcm/send/x",
		"https://fcm.googleapis.com:8443/fcm/send/x",
		"https://user@fcm.googleapis.com/fcm/send/x",
		"https://fcm.googleapis.com.evil.example/x",
		"https://evilfcm.googleapis.com/x",
		"ftp://fcm.googleapis.com/x",
		"",
	} {
		sub := d.sub
		sub.Endpoint = ep
		if _, err := r.svc.Subscribe(owner, "phone", sub, DefaultSettings()); !errors.Is(err, ErrBadEndpoint) {
			t.Errorf("%q: %v, want bad_endpoint", ep, err)
		}
	}
	for _, ep := range []string{
		"https://fcm.googleapis.com/fcm/send/x",
		"https://fcm.googleapis.com:443/wp/x",
		"https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://web.push.apple.com/x",
		"https://wns2-par02p.notify.windows.com/w/?token=x",
	} {
		if err := checkEndpoint(ep); err != nil {
			t.Errorf("%q refused: %v", ep, err)
		}
	}
	bad := d.sub
	bad.Keys.P256dh = "AAAA"
	if _, err := r.svc.Subscribe(owner, "phone", bad, DefaultSettings()); !errors.Is(err, ErrBadEndpoint) {
		t.Errorf("bad p256dh: %v", err)
	}
	bad = d.sub
	bad.Keys.Auth = "AAAA"
	if _, err := r.svc.Subscribe(owner, "phone", bad, DefaultSettings()); !errors.Is(err, ErrBadEndpoint) {
		t.Errorf("bad auth: %v", err)
	}
}

func TestSubscribeRefusesBadSettings(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	for _, q := range []Quiet{
		{Start: "22:00", End: "07:00", TZ: ""},
		{Start: "22:00", End: "07:00", TZ: "Local"},
		{Start: "22:00", End: "07:00", TZ: "Mars/Olympus"},
		{Start: "24:00", End: "07:00", TZ: "UTC"},
		{Start: "7:00", End: "08:00", TZ: "UTC"},
	} {
		set := DefaultSettings()
		set.Quiet = &q
		if _, err := r.svc.Subscribe(owner, "phone", d.sub, set); !errors.Is(err, ErrBadSettings) {
			t.Errorf("%+v: %v", q, err)
		}
	}
	set := DefaultSettings()
	set.DeskMinutes = -1
	if _, err := r.svc.Subscribe(owner, "phone", d.sub, set); !errors.Is(err, ErrBadSettings) {
		t.Errorf("negative desk minutes: %v", err)
	}
}

func TestSettingsDecodeOntoDefaults(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"done":true}`), &s); err != nil {
		t.Fatal(err)
	}
	if want := (Settings{Blocked: true, Done: true, DeskMinutes: 5}); s != want {
		t.Fatalf("%+v, want %+v", s, want)
	}
	if err := json.Unmarshal([]byte(`{"desk_minutes":0,"quiet":{"start":"22:00","end":"07:00","tz":"UTC"}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.DeskMinutes != 0 || s.Quiet == nil || s.Quiet.End != "07:00" || !s.Blocked {
		t.Fatalf("%+v", s)
	}
}

func TestFileModesAndStableKeys(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, DefaultSettings())
	expiry := r.start.Add(2 * 24 * time.Hour)
	r.svc.SetExpiry(&expiry)
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)}) // settling: saved in pending.json
	r.at(0)
	dir := filepath.Join(r.dir, "push")
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("push dir: %v %v", info.Mode(), err)
	}
	for _, name := range []string{"vapid.json", "subscriptions.json", "expiry.json", "pending.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, err, info)
		}
	}
	key := r.svc.VAPIDPublicKey()
	again := r.open()
	if again.VAPIDPublicKey() != key {
		t.Fatal("VAPID key changed on reopen")
	}
	if got, err := again.Get(owner, d.sub.Endpoint); err != nil || got != DefaultSettings() {
		t.Fatalf("subscription not reloaded: %+v %v", got, err)
	}
	// A corrupt pending.json only loses the waiting alerts.
	if err := os.WriteFile(filepath.Join(dir, "pending.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r.dir, Config{Origin: testOrigin}); err != nil {
		t.Fatalf("Open refused a corrupt pending.json: %v", err)
	}
	// A corrupt vapid.json is an error, never a silent new key.
	if err := os.WriteFile(filepath.Join(dir, "vapid.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r.dir, Config{Origin: testOrigin}); err == nil {
		t.Fatal("Open accepted a corrupt vapid.json")
	}
}

func TestOpenNeedsHTTPSOrigin(t *testing.T) {
	if _, err := Open(t.TempDir(), Config{Origin: "http://127.0.0.1:8390"}); err == nil {
		t.Fatal("Open accepted an http origin")
	}
}

func TestDefaultClientRefusesLocalAddresses(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	svc, err := Open(t.TempDir(), Config{Origin: testOrigin})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := svc.client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("the default client dialed loopback")
	}
	if hits != 0 {
		t.Fatalf("server got %d requests", hits)
	}
	for addr, want := range map[string]bool{
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "100.127.255.254": false, "0.0.0.0": false,
		"::1": false, "fe80::1": false, "fd7a:115c:a1e0::1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
		"0.1.2.3": false, "192.0.0.1": false, "192.0.2.1": false, "198.18.0.1": false, "240.0.0.1": false,
		"64:ff9b::7f00:1": false, "64:ff9b::a00:1": false, "64:ff9b:1::1": false, "::7f00:1": false,
		"2002:a00:1::1": false, "fec0::1": false,
		"142.250.1.1": true, "100.128.0.1": true, "2607:f8b0::1": true,
	} {
		if got := dialable(netip.MustParseAddr(addr)); got != want {
			t.Errorf("dialable(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestRunDrivesTimers(t *testing.T) {
	ps := newPushService(t)
	svc, err := Open(t.TempDir(), Config{Origin: testOrigin, HTTPClient: ps.client(), Settle: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	d := newDevice(t, "dev-a")
	if _, err := svc.Subscribe(owner, "phone", d.sub, noDesk()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { svc.Run(t.Context()); close(done) }()
	t.Cleanup(func() { <-done }) // t.Context ends just before cleanups run
	svc.Observe(nil)
	svc.Observe([]Agent{blocked(2)})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := ps.take(); len(got) > 0 {
			if m := decodeOne(t, svc, d, got[0]).msg; m.Kind != "blocked" {
				t.Fatalf("got %+v", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never sent the settled alert")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A clear goes only when the agent leaves blocked: blocked again with a new
// seq keeps the notification up until the new alert replaces it.
func TestReblockedWithNewSeqKeepsNotification(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk())
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	if got := r.at(3 * time.Second); len(got) != 1 {
		t.Fatalf("first alert: %d", len(got))
	}
	r.clk.Set(r.start.Add(20 * time.Second))
	r.svc.Observe([]Agent{blocked(5)})
	for _, at := range []time.Duration{20, 23, 40, 62} {
		if got := r.at(at * time.Second); len(got) != 0 {
			t.Fatalf("t=%ds: %d pushes, want none", at, len(got))
		}
	}
	got := r.decode(d, r.at(63*time.Second))
	if len(got) != 1 || got[0].msg.Kind != "blocked" {
		t.Fatalf("t=63s: %+v, want the new alert", got)
	}
}

// Opening the agent in the app counts as seeing the alert on that device:
// its subscriptions get no clear, other devices' still do, even the same
// owner's.
func TestSeenSkipsClear(t *testing.T) {
	r := newRig(t)
	phone, laptop, theirs := newDevice(t, "dev-phone"), newDevice(t, "dev-laptop"), newDevice(t, "dev-theirs")
	r.subscribe(phone, noDesk()) // node "phone"
	if _, err := r.svc.Subscribe(owner, "laptop", laptop.sub, noDesk()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Subscribe("friend@example.com", "phone", theirs.sub, noDesk()); err != nil {
		t.Fatal(err)
	}
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	if got := r.at(3 * time.Second); len(got) != 3 {
		t.Fatalf("alerts: %d, want 3", len(got))
	}
	r.svc.Seen(owner, "phone", "w1-p2")
	r.clk.Set(r.start.Add(10 * time.Second))
	r.svc.Observe([]Agent{with(blocked(0), "working", 3)})
	got := r.at(10 * time.Second)
	paths := map[string]bool{}
	for _, g := range got {
		paths[g.path] = true
	}
	if len(got) != 2 || !paths[endpointPath(laptop)] || !paths[endpointPath(theirs)] {
		t.Fatalf("clears went to %v, want the laptop and the friend's phone", paths)
	}
}

// A slow push service delays only its own device.
func TestSlowDeviceDoesNotDelayOthers(t *testing.T) {
	r := newRig(t)
	slow, fast := newDevice(t, "dev-slow"), newDevice(t, "dev-fast")
	release := make(chan struct{})
	inner := r.ps.client().Transport
	c := &http.Client{Transport: roundTripFunc(func(q *http.Request) (*http.Response, error) {
		if strings.HasSuffix(q.URL.Path, "dev-slow") {
			select {
			case <-release:
			case <-q.Context().Done():
				return nil, q.Context().Err()
			}
		}
		return inner.RoundTrip(q)
	})}
	svc, err := Open(t.TempDir(), Config{Origin: testOrigin, HTTPClient: c, Now: r.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	r.subscribe(slow, noDesk())
	r.subscribe(fast, noDesk())
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	r.clk.Set(r.start.Add(3 * time.Second))
	stepped := make(chan struct{})
	go func() { r.svc.step(r.ctx); close(stepped) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := r.ps.take(); len(got) > 0 {
			if m := r.decode(fast, got); len(m) != 1 || m[0].msg.Kind != "blocked" {
				t.Fatalf("got %+v", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fast device waited behind the slow one")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	<-stepped
	if got := r.decode(slow, r.ps.take()); len(got) != 1 {
		t.Fatalf("slow device got %d, want 1", len(got))
	}
}

// An alert still held (here by quiet hours) survives a restart: the new
// process's baseline lets it through, since the agent is still blocked.
// One already sent does not repeat.
func TestHeldAlertSurvivesRestart(t *testing.T) {
	r := newRig(t)
	held, sent := newDevice(t, "dev-held"), newDevice(t, "dev-sent")
	quiet := noDesk()
	quiet.Quiet = &Quiet{Start: "14:00", End: "16:00", TZ: "UTC"} // rigStart is 15:00 UTC
	r.subscribe(held, quiet)
	r.subscribe(sent, noDesk())
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{blocked(2)})
	if got := r.decode(sent, r.at(3*time.Second)); len(got) != 1 {
		t.Fatalf("before the restart: %d pushes, want 1 to the device outside quiet hours", len(got))
	}
	r.svc = r.open() // a restart: the hub's seqs start again
	r.svc.Observe([]Agent{blocked(7)})
	if got := r.at(30 * time.Minute); len(got) != 0 {
		t.Fatalf("inside quiet hours after the restart: %d pushes", len(got))
	}
	got := r.decode(held, r.at(61*time.Minute))
	if len(got) != 1 || got[0].msg.Kind != "blocked" {
		t.Fatalf("after quiet hours: %+v, want the held alert", got)
	}
	// And a restart once nothing waits alerts nothing.
	r.svc = r.open()
	r.svc.Observe([]Agent{blocked(9)})
	if got := r.at(3 * time.Hour); len(got) != 0 {
		t.Fatalf("after a second restart: %d pushes, want 0", len(got))
	}
}

// A name or workspace too long for one record is cut, and the push still goes.
func TestOversizedNameIsClipped(t *testing.T) {
	r := newRig(t)
	d := newDevice(t, "dev-a")
	r.subscribe(d, noDesk())
	a := blocked(2)
	a.Name, a.Workspace = strings.Repeat("名", 3000), strings.Repeat("w", 5000)
	r.svc.Observe(nil)
	r.svc.Observe([]Agent{a})
	got := r.decode(d, r.at(3*time.Second))
	if len(got) != 1 {
		t.Fatalf("got %d pushes, want 1", len(got))
	}
	m := got[0].msg
	if n := utf8.RuneCountInString(m.Title); n != maxTitle || !strings.HasSuffix(m.Title, "…") {
		t.Fatalf("title is %d runes", n)
	}
	if n := utf8.RuneCountInString(m.Body); n != maxBody || !strings.HasPrefix(m.Body, "Asks you a question · ") {
		t.Fatalf("body is %d runes: %.40q", n, m.Body)
	}
}

func TestSubscriptionCapPerOwner(t *testing.T) {
	r := newRig(t)
	var first *device
	for i := range maxPerOwner {
		d := newDevice(t, fmt.Sprintf("dev-%d", i))
		if i == 0 {
			first = d
		}
		r.subscribe(d, noDesk())
	}
	if _, err := r.svc.Subscribe(owner, "phone", newDevice(t, "dev-extra").sub, noDesk()); !errors.Is(err, ErrTooMany) {
		t.Fatalf("subscription past the cap: %v, want ErrTooMany", err)
	}
	// Changing an existing one's settings is still an upsert, and another
	// owner has a cap of its own.
	r.subscribe(first, DefaultSettings())
	if _, err := r.svc.Subscribe("friend@example.com", "tablet", newDevice(t, "dev-friend").sub, noDesk()); err != nil {
		t.Fatal(err)
	}
}

// Other principals filling the overall cap can't lock the node owner out:
// the owner's subscriptions are outside it, though still within maxPerOwner.
func TestNodeOwnerOutsideOverallCap(t *testing.T) {
	r := newRig(t)
	for i := 0; len(r.svc.subs) < maxSubs; i++ {
		login := fmt.Sprintf("tag-reader-%d@example.com/node", i/maxPerOwner)
		if _, err := r.svc.Subscribe(login, "node", newDevice(t, fmt.Sprintf("filler-%d", i)).sub, noDesk()); err != nil {
			t.Fatalf("filler %d: %v", i, err)
		}
	}
	if _, err := r.svc.Subscribe("late@example.com", "phone", newDevice(t, "late").sub, noDesk()); !errors.Is(err, ErrTooMany) {
		t.Fatalf("a non-owner past the overall cap: %v, want ErrTooMany", err)
	}
	if _, err := r.svc.SubscribeNodeOwner(owner, "phone", newDevice(t, "owner-phone").sub, noDesk()); err != nil {
		t.Fatalf("the node owner at the overall cap: %v", err)
	}
}
