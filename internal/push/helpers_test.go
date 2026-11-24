package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/push/pushtest"
)

const testOrigin = "https://herdr-pocket.example-tailnet.ts.net"

// clock is a fake Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// device is a test browser subscription with its private keys.
type device struct {
	priv *ecdh.PrivateKey
	auth []byte
	sub  Subscription
}

func newDevice(t *testing.T, name string) *device {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return &device{priv: priv, auth: auth, sub: Subscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/" + name,
		Keys: Keys{
			P256dh: base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(auth),
		},
	}}
}

// received is one request the fake push service got.
type received struct {
	path   string
	host   string // the Host the client asked for (before the rewrite)
	header http.Header
	body   []byte
}

// pushService is an httptest push service. Endpoints keep their real push
// host; the client's transport rewrites them to this server, so endpoint
// validation stays in the path.
type pushService struct {
	srv *httptest.Server

	mu     sync.Mutex
	got    []received
	status map[string]int // path -> status; default 201
}

func newPushService(t *testing.T) *pushService {
	t.Helper()
	ps := &pushService{status: map[string]int{}}
	ps.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		ps.got = append(ps.got, received{path: r.URL.Path, host: r.Host, header: r.Header.Clone(), body: body})
		code := ps.status[r.URL.Path]
		ps.mu.Unlock()
		if code == 0 {
			code = http.StatusCreated
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(ps.srv.Close)
	return ps
}

func (ps *pushService) setStatus(d *device, code int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.status[endpointPath(d)] = code
}

func endpointPath(d *device) string {
	u, _ := url.Parse(d.sub.Endpoint)
	return u.Path
}

// take returns and forgets the requests so far.
func (ps *pushService) take() []received {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	got := ps.got
	ps.got = nil
	return got
}

// client sends every request to the fake service, keeping the Host header.
func (ps *pushService) client() *http.Client {
	target, _ := url.Parse(ps.srv.URL)
	base := ps.srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Host = r.URL.Host
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// rig is a Service on a temp state dir with a fake clock and push service.
type rig struct {
	t     *testing.T
	dir   string
	clk   *clock
	ps    *pushService
	svc   *Service
	logs  *logBuf
	ctx   context.Context
	start time.Time
}

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) Logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

var rigStart = time.Date(2026, 11, 22, 15, 0, 0, 0, time.UTC)

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), clk: &clock{t: rigStart}, ps: newPushService(t), logs: &logBuf{}, ctx: t.Context(), start: rigStart}
	r.svc = r.open()
	return r
}

func (r *rig) open() *Service {
	r.t.Helper()
	svc, err := Open(r.dir, Config{Origin: testOrigin, HTTPClient: r.ps.client(), Now: r.clk.Now, Logf: r.logs.Logf})
	if err != nil {
		r.t.Fatal(err)
	}
	return svc
}

func (r *rig) subscribe(d *device, set Settings) {
	r.t.Helper()
	if _, err := r.svc.Subscribe("owner@example.com", "phone", d.sub, set); err != nil {
		r.t.Fatal(err)
	}
}

// at advances the clock to start+d and runs one step.
func (r *rig) at(d time.Duration) []received {
	r.t.Helper()
	r.clk.Set(r.start.Add(d))
	r.svc.step(r.ctx)
	return r.ps.take()
}

// decoded is a received push, decrypted and checked.
type decoded struct {
	msg     message
	urgency string
	topic   string
	path    string
}

// open decrypts every request with d's keys (each must be for d) and checks
// the transport headers and the VAPID JWT.
func (r *rig) decode(d *device, got []received) []decoded {
	r.t.Helper()
	var out []decoded
	for _, g := range got {
		if g.path != endpointPath(d) {
			r.t.Fatalf("push went to %s, want %s", g.path, endpointPath(d))
		}
		out = append(out, decodeOne(r.t, r.svc, d, g))
	}
	return out
}

func decodeOne(t *testing.T, svc *Service, d *device, g received) decoded {
	t.Helper()
	if ce := g.header.Get("Content-Encoding"); ce != "aes128gcm" {
		t.Fatalf("Content-Encoding %q", ce)
	}
	if ttl := g.header.Get("TTL"); ttl != "3600" {
		t.Fatalf("TTL %q", ttl)
	}
	verifyVAPID(t, svc, g)
	plain := decryptRFC8291(t, d, g.body)
	var m message
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("payload %q: %v", plain, err)
	}
	tp := g.header.Get("Topic")
	if tp != topic(m.Tag) || len(tp) > 32 || len(tp) == 0 {
		t.Fatalf("Topic %q for tag %q", tp, m.Tag)
	}
	if _, err := base64.RawURLEncoding.DecodeString(tp); err != nil {
		t.Fatalf("Topic %q is not base64url: %v", tp, err)
	}
	return decoded{msg: m, urgency: g.header.Get("Urgency"), topic: tp, path: g.path}
}

// decryptRFC8291 undoes the aes128gcm content coding with the device's
// private key.
func decryptRFC8291(t *testing.T, d *device, body []byte) []byte {
	t.Helper()
	plain, err := (&pushtest.Device{Priv: d.priv, Auth: d.auth}).Decrypt(body)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

// verifyVAPID checks "vapid t=<jwt>, k=<key>": k is the service's key, and
// the ES256 signature, aud and sub are right.
func verifyVAPID(t *testing.T, svc *Service, g received) {
	t.Helper()
	h := g.header.Get("Authorization")
	tok, key, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !strings.HasPrefix(h, "vapid t=") || !ok {
		t.Fatalf("Authorization %q", h)
	}
	if key != svc.VAPIDPublicKey() {
		t.Fatalf("k=%q, want the service's key", key)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	var hdr struct{ Alg, Typ string }
	var claims struct {
		Aud string `json:"aud"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	for i, v := range []any{&hdr, &claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	if hdr.Alg != "ES256" {
		t.Fatalf("alg %q", hdr.Alg)
	}
	if want := "https://" + g.host; claims.Aud != want {
		t.Fatalf("aud %q, want %q", claims.Aud, want)
	}
	if claims.Sub != testOrigin {
		t.Fatalf("sub %q, want %q", claims.Sub, testOrigin)
	}
	// webpush-go sets exp from the real clock, so only check it is ahead.
	if claims.Exp <= time.Now().Unix() {
		t.Fatalf("exp %d is not in the future", claims.Exp)
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubRaw)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: %v (%d bytes)", err, len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	rInt, sInt := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], rInt, sInt) {
		t.Fatal("VAPID signature does not verify")
	}
}
