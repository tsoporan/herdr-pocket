package server_test

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tsoporan/herdr-pocket/internal/hub"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A try that carries a drop report gets one log line saying how the app's
// last socket ended.
func TestWSLogsDropReport(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e := start(t)
	q := url.Values{"drop": {`{"code":1006,"reason":"","clean":false,"visible":false,"since_visibility_ms":42000,"online":true,"net":"wifi","lived_ms":95000,"failed_tries":0,"ago_ms":1000}`}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url+"/ws?"+q.Encode(), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {e.origin}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	read[hub.Snapshot](t, c)
	want := `the app says its last socket ended: code 1006 "", not clean after 1m35s; page hidden for 42s; online wifi; 0 failed tries; 1s ago`
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log has no drop line:\n%s", logs.String())
	}
}
