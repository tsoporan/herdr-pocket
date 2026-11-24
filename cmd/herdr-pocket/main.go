// Command herdr-pocket serves the herdr-pocket PWA and its WebSocket, and
// installs and signs in its own tailnet node.
//
//	herdr-pocket [serve] [flags]     run the server (the default)
//	herdr-pocket install [-- flags]  write and start the systemd user unit
//	herdr-pocket uninstall           stop and remove the unit
//	herdr-pocket login               sign the node in to the tailnet
//	herdr-pocket logout              remove the node from the tailnet
//	herdr-pocket status              show the service and node state
//	herdr-pocket qr                  show the app's address as a QR code for a phone
//	herdr-pocket version
//
// serve --dev is plain HTTP on a loopback address with a fake identity, for UI work:
//
//	herdr-pocket --dev [--dev-addr 127.0.0.1:8390] [--dev-origin http://localhost:5173]... [--herdr-socket PATH] [--hostname NAME]
//	             [--claude-config-dir DIR]... [--read-only] [--state-dir DIR] [--dev-role controller|reader] [--dev-expiry 9d]
//	             [--kinds claude,codex]
//
// Without --dev it serves HTTPS on :443 of its own tailnet node, to the
// node's owner plus anyone named with --allow (controllers) or --reader.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tsoporan/herdr-pocket/internal/answer"
	"github.com/tsoporan/herdr-pocket/internal/audit"
	"github.com/tsoporan/herdr-pocket/internal/herdr"
	"github.com/tsoporan/herdr-pocket/internal/hub"
	"github.com/tsoporan/herdr-pocket/internal/push"
	"github.com/tsoporan/herdr-pocket/internal/queue"
	"github.com/tsoporan/herdr-pocket/internal/server"
	"github.com/tsoporan/herdr-pocket/internal/start"
	"github.com/tsoporan/herdr-pocket/internal/tailnet"
	"github.com/tsoporan/herdr-pocket/internal/transcript"
	"github.com/tsoporan/herdr-pocket/web"
)

// commands are the subcommands other than serve. Each parses its own args.
var commands = map[string]func(ctx context.Context, args []string, stdout io.Writer) error{
	"install":   runInstall,
	"uninstall": runUninstall,
	"login":     runLogin,
	"logout":    runLogout,
	"status":    runStatus,
	"qr":        runQR,
	"version":   runVersion,
}

func main() {
	log.SetFlags(log.Ltime)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := dispatch(ctx, os.Args[1:], os.Stdout)
	stop()
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case err == errUsage: // the flag package already printed why
		os.Exit(2)
	case errors.Is(err, errUsage):
		fmt.Fprintf(os.Stderr, "herdr-pocket: %v\n", err)
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "herdr-pocket: %v\n", err)
		os.Exit(1)
	}
}

// errUsage wraps a bad command line; main exits 2 for it.
var errUsage = errors.New("usage")

// dispatch runs the subcommand named by args[0]. No args, or a first arg
// that is a flag, means serve. Any other word is a usage error, so a typo
// never starts the server.
func dispatch(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return serve(ctx, args)
	}
	if args[0] == "serve" {
		return serve(ctx, args[1:])
	}
	if run, ok := commands[args[0]]; ok {
		return run(ctx, args[1:], stdout)
	}
	return fmt.Errorf("%w: unknown command %q (want serve, install, uninstall, login, logout, status, qr or version)", errUsage, args[0])
}

// parseFlags parses args into fs and refuses positional args, which no
// command takes. fs must use flag.ContinueOnError.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: %s: unexpected argument %q", errUsage, fs.Name(), fs.Arg(0))
	}
	return nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("herdr-pocket", flag.ContinueOnError)
	dev := fs.Bool("dev", false, "serve plain HTTP on a loopback address with a fake identity, for UI work")
	devAddr := fs.String("dev-addr", "127.0.0.1:8390", "loopback address for --dev")
	var extraOrigins []string
	fs.Func("dev-origin", "another loopback browser origin the --dev WebSocket accepts, such as vite's (repeatable)", func(v string) error {
		if err := checkDevOrigin(v); err != nil {
			return err
		}
		extraOrigins = append(extraOrigins, v)
		return nil
	})
	socket := fs.String("herdr-socket", "", "herdr socket (default $HERDR_SOCKET_PATH, then ~/.config/herdr/herdr.sock)")
	hostname := fs.String("hostname", "herdr-pocket", "tailnet node name; with --dev the default host shown is this machine's hostname")
	var claudeDirs []string
	fs.Func("claude-config-dir", "a Claude Code config dir whose projects/ holds the transcripts (repeatable; searched in addition to ~/.claude and $CLAUDE_CONFIG_DIR)", func(v string) error {
		if v == "" {
			return errors.New("empty dir")
		}
		claudeDirs = append(claudeDirs, v)
		return nil
	})
	tailscaleLogs := fs.Bool("tailscale-logs", false, "upload the node's own debug logs to log.tailscale.com, as a normal Tailscale client does (off by default)")
	readOnly := fs.Bool("read-only", false, "refuse every input from the app; only read herdr")
	kindsFlag := fs.String("kinds", "claude,codex", "the agent kinds the app may start, comma-separated, from herdr's list")
	stateDir := fs.String("state-dir", tailnet.DefaultStateDir(), "directory for the tailnet node, status, audit log, push and work queue (with --dev: $XDG_RUNTIME_DIR/herdr-pocket-dev-state, and a work queue per herdr socket)")
	devRole := fs.String("dev-role", "controller", "with --dev, the fake viewer's role: controller or reader")
	var devExpiry *time.Duration
	fs.Func("dev-expiry", "with --dev, a fake node key expiry this far ahead, such as 9d or 36h", func(v string) error {
		d, err := parseDays(v)
		if err != nil {
			return err
		}
		devExpiry = &d
		return nil
	})
	var allow, readers []string
	fs.Func("allow", "another controller: a tailnet login or a tag such as tag:phone (repeatable; the node's owner is always one)", func(v string) error {
		allow = append(allow, v)
		return nil
	})
	fs.Func("reader", "a viewer who may not send input: a tailnet login or a tag (repeatable)", func(v string) error {
		readers = append(readers, v)
		return nil
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	kinds, err := parseKinds(*kindsFlag)
	if err != nil {
		return err
	}
	// A flag that the mode ignores is a mistake, most likely in an installed
	// unit's ExecStart; refuse it rather than run with other settings.
	for _, name := range []string{"dev-addr", "dev-origin", "dev-role", "dev-expiry", "allow", "reader", "tailscale-logs"} {
		devOnly := strings.HasPrefix(name, "dev-")
		if flagSet(fs, name) && devOnly != *dev {
			if devOnly {
				return fmt.Errorf("%w: --%s needs --dev", errUsage, name)
			}
			return fmt.Errorf("%w: --%s is for the tailnet, not --dev (use --dev-role)", errUsage, name)
		}
	}

	// Given dirs are searched in addition to ~/.claude and $CLAUDE_CONFIG_DIR
	// (SPEC). Tests stay hermetic by building transcript.NewReader with
	// explicit dirs; fixture session ids never match a real transcript.
	claudeDirs = transcript.DefaultConfigDirs(claudeDirs...)
	if !*dev {
		return serveTailnet(ctx, tailnetOptions{
			stateDir: *stateDir, hostname: *hostname, socket: *socket, claudeDirs: claudeDirs,
			readOnly: *readOnly, allow: allow, readers: readers, tailscaleLogs: *tailscaleLogs, kinds: kinds,
		})
	}
	host := *hostname
	if !flagSet(fs, "hostname") {
		h, err := os.Hostname()
		if err != nil {
			return err
		}
		host = h
	}
	if *devRole != string(server.Controller) && *devRole != string(server.Reader) {
		return fmt.Errorf("%w: --dev-role %q: want controller or reader", errUsage, *devRole)
	}
	opts := devOptions{role: server.Role(*devRole), expiry: devExpiry, stateDir: *stateDir, kinds: kinds}
	if !flagSet(fs, "state-dir") {
		// --dev never touches the real node's state dir.
		opts.stateDir = devStateDir()
	}
	return runDev(ctx, *devAddr, *socket, host, extraOrigins, claudeDirs, *readOnly, opts)
}

// devStateDir is --dev's default state dir: the audit log goes there.
func devStateDir() string {
	// Never the shared os.TempDir(): another user could create the path first.
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return tailnet.DefaultStateDir() + "-dev"
		}
		dir = cache
	}
	return filepath.Join(dir, "herdr-pocket-dev-state")
}

// parseDays is time.ParseDuration plus whole days ("9d").
func parseDays(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("%q: want a number of days such as 9d", v)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(v)
}

// devOptions are the --dev-only knobs for exercising the UI.
type devOptions struct {
	role     server.Role
	expiry   *time.Duration // nil: key expiry off
	stateDir string
	kinds    []hub.StartKind // --kinds
}

// The fake tailnet users --dev serves: the node's owner, or a reader.
const devOwnerID = 1

var (
	devOwner  = tailnet.Peer{Login: "owner@dev.invalid", UserID: devOwnerID, Node: "dev"}
	devReader = tailnet.Peer{Login: "reader@dev.invalid", UserID: 2, Node: "dev"}
)

// devIdentity answers every WhoIs with one fake peer; --dev listens only on
// loopback.
type devIdentity struct{ peer tailnet.Peer }

func (d devIdentity) WhoIs(context.Context, string) (tailnet.Peer, error) { return d.peer, nil }

func runVersion(_ context.Context, args []string, stdout io.Writer) error {
	if err := parseFlags(flag.NewFlagSet("version", flag.ContinueOnError), args); err != nil {
		return err
	}
	version := "(unknown)"
	if info, ok := debug.ReadBuildInfo(); ok {
		version = info.Main.Version
	}
	fmt.Fprintln(stdout, "herdr-pocket", version)
	return nil
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// checkDevAddr accepts only a loopback IP or localhost with an explicit port
// that isn't reserved.
func checkDevAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--dev-addr %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--dev-addr %q: only loopback addresses are allowed", addr)
	}
	return nil
}

// checkDevOrigin accepts only an exact http origin on a loopback host, so
// --dev-origin can't open the dev WebSocket to other sites.
func checkDevOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("--dev-origin %q: %w", origin, err)
	}
	if u.Scheme != "http" || u.Host == "" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("--dev-origin %q: want http://HOST:PORT", origin)
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--dev-origin %q: only loopback origins are allowed", origin)
	}
	return nil
}

func runDev(ctx context.Context, addr, socketFlag, host string, extraOrigins, claudeDirs []string, readOnly bool, opts devOptions) error {
	if err := checkDevAddr(addr); err != nil {
		return err
	}
	socket, err := herdr.ResolveSocket(socketFlag)
	if err != nil {
		return err
	}
	client := herdr.NewClient(socket)
	pong, err := client.CheckProtocol(ctx)
	if err != nil {
		return fmt.Errorf("herdr at %s: %w", socket, err)
	}
	auditLog, err := audit.Open(opts.stateDir)
	if err != nil {
		return err
	}
	defer auditLog.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close() // for the returns before serveHTTP; a second close is harmless
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := server.Config{
		Origins:  append(devOrigins(port), extraOrigins...),
		Input:    client,
		Identity: devIdentity{peer: devOwner},
		Owner:    devOwnerID,
		Audit:    auditLog,
	}
	if opts.role == server.Reader {
		cfg.Identity, cfg.Readers = devIdentity{peer: devReader}, []string{devReader.Login}
	}
	if readOnly {
		cfg.Input = nil
	}
	canAct, err := server.CanAct(cfg)
	if err != nil {
		return err
	}
	h := hub.New(client, host, transcript.NewReader(claudeDirs...))
	// Push works in --dev, with its keys and subscriptions in the dev state
	// dir. A Chrome page on loopback can subscribe, so its pushes are real.
	pusher, err := push.Open(opts.stateDir, push.Config{Origin: devPushOrigin, Logf: log.Printf, Question: h.Question, CanAct: canAct})
	if err != nil {
		return err
	}
	cfg.Push = pusher
	if opts.expiry != nil {
		at := time.Now().Add(*opts.expiry)
		h.SetKeyExpiry(&at)
	}
	h.SetPusher(pusher)
	q, err := openQueue(devQueueDir(opts.stateDir, socket), h, client, auditLog, readOnly, canAct)
	if err != nil {
		return err
	}
	cfg.Queue = q
	if !readOnly {
		cfg.Starts = openStarts(h, client, q, auditLog, opts.kinds)
	}
	if err := h.Start(ctx); err != nil {
		return fmt.Errorf("herdr at %s: %w", socket, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Go(func() { pusher.Run(ctx) })
	if !readOnly {
		wg.Go(func() { q.Run(ctx) })
		wg.Go(func() { cfg.Starts.Run(ctx) })
	}

	handler, err := server.New(h, web.Dist, cfg)
	if err != nil {
		return err
	}
	mode := ""
	if readOnly {
		mode = ", read-only"
	}
	log.Printf("herdr-pocket --dev on http://%s as %s (herdr %s, protocol %d%s; transcripts from %s; audit in %s)", ln.Addr(), opts.role, pong.Version, pong.Protocol, mode, strings.Join(claudeDirs, ", "), opts.stateDir)
	return serveHTTP(ctx, ln, handler)
}

// devPushOrigin is --dev's VAPID subscriber. push.Open wants an https URL
// (webpush-go would make anything else a mailto:), and the dev page's own
// origin is plain http, so this is a name that can't resolve.
const devPushOrigin = "https://herdr-pocket-dev.invalid"

// devOrigins are the browser origins of the dev server's own page.
func devOrigins(port string) []string {
	return []string{"http://127.0.0.1:" + port, "http://localhost:" + port}
}

// devQueueDir is where --dev keeps its work queue: a dir per herdr socket
// under the state dir. One --dev state dir serves fake-herdr (just dev) and
// the real herdr, whose pane ids look alike, so a list lined up against one
// must never be sent to the other.
func devQueueDir(stateDir, socket string) string {
	if abs, err := filepath.Abs(socket); err == nil {
		socket = abs
	}
	sum := sha256.Sum256([]byte(socket))
	return filepath.Join(stateDir, "dev-queue", hex.EncodeToString(sum[:8]))
}

// openQueue opens the work queue in stateDir, sending through client and
// auditing each send, and attaches it to h. A --read-only server opens it
// only to show the items: the caller must not Run it, and its Prompt refuses
// anyway, so lined-up items never reach herdr from run-real.
func openQueue(stateDir string, h *hub.Hub, client *herdr.Client, auditLog *audit.Log, readOnly bool, canAct func(push.Who) bool) (*queue.Queue, error) {
	prompt := client.Prompt
	if readOnly {
		prompt = func(context.Context, string, string) error { return errReadOnlyQueue }
	}
	q, err := queue.Open(stateDir, queue.Config{
		Settle:   queueSettle,
		Prompt:   prompt,
		OnChange: h.QueueChanged,
		OnSent:   server.AuditQueued(auditLog),
		// An item's author must still be a controller when it is sent.
		CanSend: func(a queue.Author) bool {
			return canAct(push.Who{Login: a.Login, Node: a.Node, UserID: a.UserID, Tags: a.Tags})
		},
		// Never type into an open dialog: herdr can call an agent idle while
		// Claude Code's question is still up.
		Clear: func(ctx context.Context, paneID string) bool {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			res, err := client.Read(ctx, paneID, herdr.SourceVisible, 0)
			return err == nil && !answer.DialogOpen(res.Text)
		},
		Logf: log.Printf,
	})
	if err != nil {
		return nil, err
	}
	h.SetQueue(q)
	return q, nil
}

var errReadOnlyQueue = errors.New("read-only: the queue does not send")

// queueSettle is the queue's settle; zero is its default. Tests shorten it.
var queueSettle time.Duration

// kindLabels are the app's names for agent kinds; any other kind shows as
// itself, capitalised (kimi is Kimi).
var kindLabels = map[string]string{
	"claude": "Claude Code", "codex": "Codex", "gemini": "Gemini CLI", "copilot": "Copilot CLI",
	"opencode": "OpenCode", "qwen": "Qwen Code", "cursor": "Cursor",
}

// parseKinds reads --kinds: a comma-separated list of herdr's agent kinds,
// each at most once. Kinds aren't looked up on PATH: the service's PATH
// isn't the pane's, and a kind's executable isn't always its name.
func parseKinds(v string) ([]hub.StartKind, error) {
	var out []hub.StartKind
	seen := map[string]bool{}
	for k := range strings.SplitSeq(v, ",") {
		k = strings.TrimSpace(k)
		switch {
		case !slices.Contains(herdr.Kinds, k):
			return nil, fmt.Errorf("%w: --kinds: %q is not a herdr agent kind (want some of %s)", errUsage, k, strings.Join(herdr.Kinds, ", "))
		case seen[k]:
			return nil, fmt.Errorf("%w: --kinds: %q given twice", errUsage, k)
		}
		seen[k] = true
		label := kindLabels[k]
		if label == "" {
			label = strings.ToUpper(k[:1]) + k[1:]
		}
		out = append(out, hub.StartKind{Kind: k, Label: label})
	}
	return out, nil
}

// openStarts makes the start runner and attaches it to h. It starts agents
// through client, audits each step, and lines a task up in q for an agent
// that asks something first. Only a server that takes input makes one.
func openStarts(h *hub.Hub, client *herdr.Client, q *queue.Queue, auditLog *audit.Log, kinds []hub.StartKind) *start.Runner {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = k.Kind
	}
	r := start.New(start.Config{
		Herdr:     client,
		Kinds:     names,
		Workspace: h.WorkspacePath,
		NameLive:  h.NameLive,
		HasPane:   h.Has,
		Relist:    h.Relist,
		LineUp: func(by queue.Author, paneID, text string) error {
			_, err := q.Add(by, queue.Target{PaneID: paneID}, text)
			return err
		},
		OnChange: h.StartsChanged,
		OnStep:   server.AuditStart(auditLog),
		Logf:     log.Printf,
	})
	h.SetStarts(r, kinds)
	return r
}
