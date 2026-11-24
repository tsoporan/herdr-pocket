# herdr-pocket recipes. Fake-herdr always gets an explicit temp socket:
# $HERDR_SOCKET_PATH is the real herdr.

fake_socket := env("XDG_RUNTIME_DIR") / "herdr-pocket-fake.sock"
demo_script := "internal/fakeherdr/testdata/demo-script.json"
# The fixture agents' transcripts. ~/.claude is searched too, read-only; the
# fixtures' invented session ids never match a real transcript.
claude_fixtures := "internal/fakeherdr/testdata/claude"
# just dev's copy of them, with the times moved up to now so ages read minutes.
claude_fresh := env("XDG_RUNTIME_DIR") / "herdr-pocket-fake-claude"
dev_addr := "127.0.0.1:8390"
vite_port := "5173"
vite_origin := "http://localhost:" + vite_port
vite_origin_ip := "http://127.0.0.1:" + vite_port
# --dev writes its audit log here, never in the real node's state dir.
dev_state := env("XDG_RUNTIME_DIR") / "herdr-pocket-dev-state"

# List recipes
default:
    @just --list

# Format Go code
fmt:
    gofmt -w cmd internal web/embed.go

# go vet and svelte-check
lint:
    go vet ./...
    cd web && bun run check

# Go tests (race) and web unit tests
test:
    go test -race ./...
    cd web && bun test

# Check that this machine has what the build and the service need
doctor:
    #!/usr/bin/env bash
    set -uo pipefail
    bad=0
    ok() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
    no() { printf '  \033[31m✗\033[0m %s\n    %s\n' "$1" "$2"; bad=1; }
    if command -v go >/dev/null; then ok "Go $(go env GOVERSION | sed -e s/^go// -e s/-.*//)"; else no "Go" "Install Go 1.27 or newer: https://go.dev/dl"; fi
    if command -v bun >/dev/null; then ok "Bun $(bun --version)"; else no "Bun" "Install Bun, which builds the web app: https://bun.sh"; fi
    if command -v herdr >/dev/null; then
        if herdr status server 2>/dev/null | grep -q '^status: running'; then ok "herdr $(herdr --version | cut -d' ' -f2), running"
        else no "herdr is installed but not running" "Start it with \`herdr\`; herdr-pocket reads its agents from herdr's socket."; fi
    else no "herdr" "Install herdr, the terminal workspace herdr-pocket remote-controls: https://herdr.dev"; fi
    if systemctl --user show-environment >/dev/null 2>&1; then ok "systemd user services"
    else no "systemd user services" "\`just install\` runs herdr-pocket as a systemd user service; without one, run \`bin/herdr-pocket\` yourself."; fi
    if [ "$bad" = 0 ]; then echo "Ready for \`just install\`."; else exit 1; fi

# Build the web bundle, then the binaries into ./bin
build:
    cd web && bun install --frozen-lockfile && bun run build
    go build -o bin/ ./cmd/...

# Install herdr-pocket on PATH (go install, so $GOBIN) and (re)install the
# systemd user service to run that copy; dev builds in ./bin leave it alone
install: doctor build
    go install ./cmd/herdr-pocket
    "$(go env GOBIN)/herdr-pocket" install

# Run fake-herdr on a temp socket, replaying the demo script
fake:
    go run ./cmd/fake-herdr --socket {{fake_socket}} --script {{demo_script}}

# fake-herdr + the --dev server + vite dev, stopped together on exit
dev:
    #!/usr/bin/env bash
    set -euo pipefail
    trap 'kill $(jobs -p) 2>/dev/null' EXIT
    go build -o bin/ ./cmd/...
    # A socket left by a crashed run would pass the wait below too early.
    rm -f {{fake_socket}}
    bin/fake-herdr --socket {{fake_socket}} --script {{demo_script}} --transcripts {{claude_fixtures}} --transcripts-out {{claude_fresh}} &
    fake_pid=$!
    until [ -S {{fake_socket}} ]; do
        kill -0 $fake_pid 2>/dev/null || { echo "fake-herdr exited" >&2; exit 1; }
        sleep 0.1
    done
    # The demo script starts claude ready, codex at the trust dialog (asks
    # first) and gemini not installed (a timeout), all after 4 s.
    bin/herdr-pocket --dev --dev-addr {{dev_addr}} --kinds claude,codex,gemini --dev-origin {{vite_origin}} --dev-origin {{vite_origin_ip}} --herdr-socket {{fake_socket}} --claude-config-dir {{claude_fresh}} --hostname studio --state-dir {{dev_state}} &
    cd web && bun run dev --host 127.0.0.1 --port {{vite_port}} --strictPort

# The --dev server against the real herdr, read-only: it refuses all input
run-real: build
    bin/herdr-pocket --dev --dev-addr {{dev_addr}} --read-only --state-dir {{dev_state}}

# No --dev: the node signs in with a link and serves HTTPS on the tailnet.
# Agents and tests never run this.

# COORDINATOR ONLY: starts a real tailnet node in the real state dir
[confirm("This starts a real tailnet node in the real state dir. Continue?")]
serve-tailnet *args: build
    bin/herdr-pocket serve {{args}}
