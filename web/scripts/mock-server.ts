// A mock of the herdr-pocket server's /ws for UI work without Go or herdr.
// It speaks the M3 contract with the mockups' invented data:
//
//   bun scripts/mock-server.ts            # ws on 127.0.0.1:8390, what `bun run dev` proxies to
//   PORT=8391 bun scripts/mock-server.ts
//   READ_ONLY=1 ...                       # every mutation acks read_only
//   ROLE=reader ...                       # view only; every mutation acks forbidden
//   EXPIRY_DAYS=9 ...                     # the key-expiry banner, 9 days left
//
// Push (M5): the snapshot carries a fresh P-256 key, and the push.* messages
// keep settings in memory by endpoint. push.test sends nothing; it acks ok.
//
// It also serves dist/ with the production CSP, so `bun run build` then
// http://127.0.0.1:8390 shows CSP errors in the console like the real server.
//
// Scripted behaviour: answering or sending a digit to a blocked agent sets it
// working; prompting a blocked agent acks agent_blocked; the first answer to
// "Migrate to Postgres 18" acks question_changed with a new question; the
// first prompt to "Investigate memory leak" finds it just blocked (the race
// that makes a prompt ack agent_blocked); Esc stops a working agent; "Add rate
// limiting to /v2" counts its tests up.
//
// Queue (herd pass 2): "Add rate limiting to /v2" starts with the mockup's
// list. queue.add and queue.remove act on it; when an agent with waiting
// items goes idle or done, its oldest one is sent 5 s later and picked up.
//
// Start (herd pass 3): each step takes 1.2 s. A task with "trust" in it asks
// first (the new agent is blocked); kind codex times out (not installed);
// workspace docs is gone (workspace_not_found before a tab opens); else done.
// A known job_id returns that job, as the server does.

import { join } from "node:path";
import type { ServerWebSocket } from "bun";
import type {
  AckError,
  Agent,
  Detail,
  Expiry,
  Patch,
  Question,
  ClientRequest,
  PaneRequest,
  PushRequest,
  PushSettings,
  QueueItem,
  QueueRequest,
  Role,
  ServerMessage,
  Snapshot,
  StartJob,
  StartKind,
  StartRequest,
  Status,
  Watch,
  Workspace,
} from "../src/lib/protocol";

const PORT = Number(process.env.PORT ?? 8390);
const READ_ONLY = process.env.READ_ONLY === "1";
const ROLE: Role = process.env.ROLE === "reader" ? "reader" : "controller";
const EXPIRY: Expiry | null =
  process.env.EXPIRY_DAYS === undefined
    ? null
    : {
        at: new Date(Date.now() + Number(process.env.EXPIRY_DAYS) * 86_400_000).toISOString(),
        days_left: Number(process.env.EXPIRY_DAYS),
      };
const CSP = "default-src 'self'; frame-ancestors 'none'";
const VAPID_KEY = await (async () => {
  const pair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign"]);
  const raw = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  return Buffer.from(raw).toString("base64url");
})();
const DIST = join(import.meta.dir, "..", "dist");

const ago = (min: number) => new Date(Date.now() - min * 60_000).toISOString();

let seq = 100;
function agent(pane_id: string, name: string, status: Status, line: string, ws: string, min: number, kind = "claude"): Agent {
  return {
    pane_id,
    name,
    herdr_name: name,
    agent: kind,
    status,
    line,
    workspace_id: pane_id.split(":")[0]!,
    workspace_label: ws,
    tab_id: `${pane_id.split(":")[0]}:t1`,
    changed_at: ago(min),
    state_change_seq: seq++,
    tokens: {},
    focused: false,
  };
}

const agents = new Map<string, Agent>(
  [
    agent("w1:p1", "Refactor auth middleware", "blocked", "Wants to run a command", "api", 2),
    agent("w2:p1", "Migrate to Postgres 18", "blocked", "Asks: staging or dev first?", "infra", 6),
    agent("w3:p3", "Tidy the release script", "blocked", "Waiting for you", "web", 9, "codex"),
    agent("w3:p1", "Fix flaky checkout test", "done", "Finished · PR #48 is ready", "web", 0.7),
    agent("w1:p2", "Add rate limiting to /v2", "working", "Running tests · 14m", "api", 14),
    agent("w2:p2", "Investigate memory leak", "working", "Reading the WebSocket hub", "infra", 4),
    agent("w3:p2", "Dark mode for settings", "working", "Checking colour contrast", "web", 2, "codex"),
    agent("w4:p1", "Write onboarding guide", "idle", "Waiting for your next task", "docs", 22),
    agent("w1:p3", "Bump deps and changelog", "idle", "PR #47 merged", "api", 60),
  ].map((a) => [a.pane_id, a]),
);

const permission: Question = {
  id: "q-auth-1",
  kind: "permission",
  title: "Allow this command?",
  where: "~/api",
  command: "rm -rf dist && bun run build",
  led_here:
    "The auth tests pass (212 of 212). It wants to rebuild the bundle to check its size, which deletes the old build first.",
  options: [
    { key: "1", label: "Allow once", sub: "Runs this command now", primary: true, needs_note: false, own: false },
    {
      key: "2",
      label: "Always allow",
      sub: "Won’t ask again for rm -rf dist in this project",
      primary: false,
      needs_note: false,
      own: false,
    },
    { key: "3", label: "Don’t allow", sub: "Then tell it what to do instead", primary: false, needs_note: true, own: false },
  ],
};

function postgres(id: string, first: string): Question {
  return {
    id,
    kind: "ask",
    title: "Staging or dev first?",
    where: "~/infra",
    command: null,
    led_here: "The migration scripts are ready and dry-run cleanly against a copy of production.",
    options: [
      { key: "1", label: first, sub: "Runs the migration there first", primary: true, needs_note: false, own: false },
      { key: "2", label: "Dev first", sub: "Runs the migration on dev first", primary: false, needs_note: false, own: false },
      { key: "3", label: "Something else", sub: "Then tell it what you want", primary: false, needs_note: true, own: false },
    ],
  };
}

const unknownScreen = `Tidy the release script
  Overwrite CHANGELOG.md? This can't be undone.
  [y] yes   [n] no   [d] show diff`;

const details = new Map<string, Detail>();
function detail(pane_id: string, d: Partial<Detail>): void {
  const a = agents.get(pane_id)!;
  details.set(pane_id, {
    type: "detail",
    pane_id,
    source: a.agent === "claude" ? "transcript" : "terminal",
    agent_label: a.agent === "claude" ? "Claude Code" : "Codex",
    status_since: a.changed_at,
    now: "",
    last_message: null,
    asks: false,
    steps: [],
    context_pct: null,
    pr: null,
    question: null,
    ...d,
  });
}

let tests = 61;
function rateLimitSteps(): Detail["steps"] {
  return [
    {
      kind: "you",
      text: "Add a token-bucket rate limit to every /v2 route, 100 requests a minute per key",
      meta: "You · 14 min ago",
    },
    { kind: "ok", text: "Read the v2 router", meta: "src/routes/v2/index.ts" },
    { kind: "ok", text: "Created the rate-limit middleware", meta: "src/middleware/limit.ts · 58 lines" },
    { kind: "ok", text: "Added it to the v2 router", meta: "src/routes/v2/index.ts · 1 line changed" },
    { kind: "ok", text: "Ran the unit tests: 18 passed", meta: "0.4 s" },
    { kind: "now", text: "Running the integration tests", meta: `${tests} of 140 so far, none failing` },
  ];
}

detail("w1:p2", {
  now: "Running the integration tests for the v2 routes, to check the new limit doesn't break existing clients.",
  steps: rateLimitSteps(),
  context_pct: 27,
  pr: "#51 · checks running",
});
detail("w1:p1", { now: "Rebuilding the bundle to check its size.", question: permission, context_pct: 41 });
detail("w2:p1", { now: "Asking where to run the migration first.", question: postgres("q-pg-1", "Staging first") });
detail("w3:p3", {
  now: "Overwrite CHANGELOG.md?",
  question: {
    id: "q-tidy-1",
    kind: "unknown",
    title: "It's waiting for you",
    where: null,
    command: null,
    led_here: null,
    options: [],
    screen: unknownScreen,
  },
});
detail("w3:p1", {
  now: "The checkout test was racing the cart fetch. It now waits for the cart, and the PR is ready.",
  steps: [
    { kind: "you", text: "Find out why the checkout test fails one run in ten", meta: "You · 31 min ago" },
    { kind: "ok", text: "Ran the checkout test 50 times: 5 failed", meta: "tests/checkout.spec.ts" },
    { kind: "fail", text: "Ran the e2e suite: 1 failed", meta: "checkout › applies coupon" },
    { kind: "ok", text: "Changed the checkout test", meta: "tests/checkout.spec.ts · 4 lines changed" },
    { kind: "ok", text: "Ran the checkout test 50 times: 50 passed", meta: "38 s" },
  ],
  context_pct: 63,
  pr: "#48 · ready to review",
  last_message: `The checkout test was racing the cart fetch: it clicked **Pay** before \`GET /cart\` came back, so about one run in ten saw an empty cart.

What I changed:
- The test now waits for \`cart-ready\` before it clicks.
- \`applyCoupon\` no longer swallows a 409, so a real failure shows up.
* The e2e suite passed 50 times in a row.

To check it yourself:
1. Run \`bun test tests/checkout.spec.ts\` a few times.
2. Look at the diff in tests/checkout.spec.ts (4 lines).
3. Then merge PR #48.

Nothing else touches the cart fetch, so I left the app code alone. <b>This stays text.</b>

Want me to open a follow-up PR that adds the same wait to the other two cart tests?`,
  asks: true,
});
for (const id of ["w2:p2", "w3:p2", "w4:p1", "w1:p3"]) detail(id, { now: agents.get(id)!.line + "." });
detail("w1:p3", {
  now: "PR #47 merged.",
  last_message: "All the dependencies are on their latest minor versions and the changelog has a 2.4.0 entry. PR #47 is merged; nothing is left on this branch.",
  context_pct: 18,
  pr: "#47 merged",
});
detail("w4:p1", {
  now: "Waiting for your next task.",
  last_message: "The onboarding guide is in docs/onboarding.md. Should the setup section cover Windows too?",
  asks: true,
  context_pct: 9,
});
detail("w3:p2", {
  now: "Checking colour contrast",
  last_message: `• Checked the settings page in dark mode
  └ 12 of 14 pairs pass AA contrast
• Fixing the two muted labels
  └ src/settings/theme.css`,
});

function screen(pane_id: string): string {
  const a = agents.get(pane_id)!;
  if (pane_id === "w1:p2")
    return `● Bash(bun test tests/integration/v2)
  ⎿ ${tests}/140 · 0 fail
✻ Running tests… (14m · esc to interrupt)
  ⏵⏵ auto mode on`;
  if (pane_id === "w3:p3") return unknownScreen;
  if (a.status === "blocked") return `${a.name}\n\n❯ 1. Yes\n  2. No`;
  return `${a.name}\n> ${a.line}\n`;
}

// Sockets and what each one watches.
let queueSeq = 1;
function queueItem(pane_id: string, text: string, state: QueueItem["state"], addedMin: number, sentMin?: number): QueueItem {
  return {
    id: `q${queueSeq++}`,
    target: { pane_id },
    text,
    by: "fixture@example.test",
    added: ago(addedMin),
    state,
    ...(sentMin === undefined ? {} : { sent_at: ago(sentMin), picked_up_at: ago(sentMin) }),
  };
}
let queue: QueueItem[] = [
  queueItem("w1:p2", "Run the linter and fix what it finds", "picked_up", 40, 31),
  queueItem("w1:p2", "Then update the v2 docs with the new limit", "waiting", 20),
  queueItem("w1:p2", "Open a draft PR when the tests pass", "waiting", 18),
];

const watchers = new Map<ServerWebSocket<unknown>, Set<string>>();

function send(ws: ServerWebSocket<unknown>, msg: ServerMessage): void {
  ws.send(JSON.stringify(msg));
}

function publishPane(pane_id: string): void {
  const a = agents.get(pane_id)!;
  const patch: Patch = { type: "patch", agents: { [pane_id]: a } };
  for (const [ws, watched] of watchers) {
    send(ws, patch);
    if (watched.has(pane_id)) {
      send(ws, details.get(pane_id)!);
      send(ws, { type: "output", pane_id, text: screen(pane_id) });
    }
  }
}

function publishQueue(): void {
  for (const ws of watchers.keys()) send(ws, { type: "patch", queue });
}

function setItem(id: string, change: Partial<QueueItem>): void {
  queue = queue.map((it) => (it.id === id ? { ...it, ...change } : it));
  publishQueue();
}

// The mock's sending rule: idle or done for 5 s, then the oldest waiting item goes.
function sendNext(pane_id: string): void {
  const next = queue.find((it) => it.state === "waiting" && "pane_id" in it.target && it.target.pane_id === pane_id);
  if (!next) return;
  setTimeout(() => {
    const a = agents.get(pane_id);
    if (!a || (a.status !== "idle" && a.status !== "done")) return;
    if (!queue.some((it) => it.id === next.id && it.state === "waiting")) return;
    setItem(next.id, { state: "sent", sent_at: new Date().toISOString() });
    setStatus(pane_id, "working", "Started from your list");
    setItem(next.id, { state: "picked_up", picked_up_at: new Date().toISOString() });
  }, 5000);
}

function handleQueue(ws: ServerWebSocket<unknown>, req: QueueRequest): void {
  const fail = (code: AckError["code"], message: string) => send(ws, { type: "ack", id: req.id, ok: false, error: { code, message } });
  if (ROLE === "reader") return fail("forbidden", "view only");
  if (READ_ONLY) return fail("read_only", "the server is read-only");
  if (req.type === "queue.remove") {
    const it = queue.find((q) => q.id === req.item_id);
    if (!it) return fail("not_found", "no such item");
    if (it.state !== "waiting") return fail("not_waiting", "already sent");
    queue = queue.filter((q) => q.id !== req.item_id);
  } else {
    const text = req.text.trim();
    if (!("pane_id" in req.target) || !agents.has(req.target.pane_id)) return fail("not_found", "no such pane");
    if (!text || new TextEncoder().encode(text).length > 4000) return fail("invalid_request", "text must be 1-4000 bytes");
    const pane_id = req.target.pane_id;
    if (queue.filter((q) => q.state === "waiting" && "pane_id" in q.target && q.target.pane_id === pane_id).length >= 20)
      return fail("queue_full", "20 waiting");
    queue = [...queue, queueItem(pane_id, text, "waiting", 0)];
  }
  send(ws, { type: "ack", id: req.id, ok: true });
  publishQueue();
}

function setStatus(pane_id: string, status: Status, line: string): void {
  const a = agents.get(pane_id)!;
  agents.set(pane_id, { ...a, status, line, changed_at: new Date().toISOString(), state_change_seq: seq++ });
  const d = details.get(pane_id)!;
  details.set(pane_id, {
    ...d,
    status_since: agents.get(pane_id)!.changed_at,
    question: status === "blocked" ? d.question : null,
  });
  publishPane(pane_id);
  if (status === "idle" || status === "done") sendNext(pane_id);
}

type Result = { ok: true } | { ok: false; code: "agent_blocked" | "question_changed" | "read_only" | "forbidden" | "not_found"; message: string };

let postgresAsked = false;
let leakBlocked = false;

const pushSettings = new Map<string, PushSettings>();

function handlePush(ws: ServerWebSocket<unknown>, req: PushRequest): void {
  switch (req.type) {
    case "push.subscribe":
      if (!req.subscription.endpoint.startsWith("https://"))
        return send(ws, { type: "ack", id: req.id, ok: false, error: { code: "bad_endpoint", message: "not a push service" } });
      pushSettings.set(req.subscription.endpoint, req.settings);
      return send(ws, { type: "ack", id: req.id, ok: true, settings: req.settings });
    case "push.get": {
      const settings = pushSettings.get(req.endpoint);
      return send(
        ws,
        settings
          ? { type: "ack", id: req.id, ok: true, settings }
          : { type: "ack", id: req.id, ok: false, error: { code: "not_found", message: "no such subscription" } },
      );
    }
    case "push.unsubscribe":
      pushSettings.delete(req.endpoint);
      return send(ws, { type: "ack", id: req.id, ok: true });
    case "push.test":
      if (!pushSettings.has(req.endpoint))
        return send(ws, { type: "ack", id: req.id, ok: false, error: { code: "not_found", message: "no such subscription" } });
      return send(ws, { type: "ack", id: req.id, ok: true });
  }
}

function handle(req: PaneRequest): Result {
  const a = agents.get(req.pane_id);
  if (!a) return { ok: false, code: "not_found", message: `no pane ${req.pane_id}` };
  if (ROLE === "reader") return { ok: false, code: "forbidden", message: "view only" };
  if (READ_ONLY) return { ok: false, code: "read_only", message: "the server is read-only" };
  const d = details.get(req.pane_id)!;
  switch (req.type) {
    case "prompt":
      if (req.pane_id === "w2:p2" && !leakBlocked) {
        leakBlocked = true;
        details.set(req.pane_id, { ...d, question: { ...permission, id: "q-leak-1", where: "~/infra", command: "kill -USR1 $(pgrep hub)", led_here: "It wants a heap dump from the running hub." } });
        setStatus(req.pane_id, "blocked", "Wants to run a command");
      }
      if (agents.get(req.pane_id)!.status === "blocked") return { ok: false, code: "agent_blocked", message: "the agent is blocked" };
      details.set(req.pane_id, { ...d, asks: false, steps: [...d.steps, { kind: "you", text: req.text, meta: "You · now" }] });
      setStatus(req.pane_id, "working", "Thinking");
      return { ok: true };
    case "keys": {
      const key = req.keys[0] ?? "";
      if (a.status === "blocked" && /^[1-9]$/.test(key)) setStatus(req.pane_id, "working", "Working again");
      else if (key === "esc" && a.status === "working") setStatus(req.pane_id, "idle", "Stopped");
      return { ok: true };
    }
    case "answer":
      if (a.status !== "blocked" || d.question?.id !== req.question_id)
        return { ok: false, code: "question_changed", message: "the question changed" };
      if (req.pane_id === "w2:p1" && !postgresAsked) {
        postgresAsked = true;
        details.set(req.pane_id, { ...d, question: postgres("q-pg-2", "Staging first, then dev") });
        publishPane(req.pane_id);
        return { ok: false, code: "question_changed", message: "the question changed" };
      }
      setStatus(req.pane_id, "working", req.note ? `Working on: ${req.note}` : "Working again");
      return { ok: true };
    case "rename":
      agents.set(req.pane_id, { ...a, name: req.name, herdr_name: req.name });
      publishPane(req.pane_id);
      return { ok: true };
    case "text":
    case "focus_on_desktop":
      return { ok: true };
  }
}

const PATHS: Record<string, string> = { w1: "~/code/api", w2: "~/code/infra", w3: "" };
function workspaceRows(): Workspace[] {
  return ["api", "infra", "web", "docs"].map((label, i) => {
    const workspace_id = `w${i + 1}`;
    return {
      workspace_id,
      label,
      number: i + 1,
      path: PATHS[workspace_id] ?? "~/code/docs",
      agent_count: [...agents.values()].filter((a) => a.workspace_id === workspace_id).length,
    };
  });
}

const START_KINDS: StartKind[] = [
  { kind: "claude", label: "Claude Code" },
  { kind: "codex", label: "Codex" },
];
let starts: StartJob[] = [];
let paneSeq = 7;

function publishStarts(): void {
  for (const ws of watchers.keys()) send(ws, { type: "patch", starts });
}

function setJob(id: string, change: Partial<StartJob>): void {
  starts = starts.map((j) => (j.id === id ? { ...j, ...change } : j));
  publishStarts();
}

function handleStart(ws: ServerWebSocket<unknown>, req: StartRequest): void {
  const fail = (code: AckError["code"], message: string) => send(ws, { type: "ack", id: req.id, ok: false, error: { code, message } });
  if (ROLE === "reader") return fail("forbidden", "view only");
  if (READ_ONLY) return fail("read_only", "the server is read-only");
  if (!/^[A-Za-z0-9_-]{8,64}$/.test(req.job_id)) return fail("invalid_request", "bad job_id");
  if (starts.some((j) => j.id === req.job_id)) return send(ws, { type: "ack", id: req.id, ok: true });
  if (!workspaceRows().some((w) => w.workspace_id === req.workspace_id)) return fail("not_found", "no such workspace");
  if (!START_KINDS.some((k) => k.kind === req.kind)) return fail("unknown_kind", "not offered");
  if (!/^[a-z][a-z0-9_-]{0,31}$/.test(req.name)) return fail("invalid_name", "bad name");
  if ([...agents.values()].some((a) => a.name === req.name) || starts.some((j) => j.state === "running" && j.name === req.name))
    return fail("name_taken", "taken");
  const task = req.task.trim();
  if (!task) return fail("invalid_request", "empty task");
  if (new TextEncoder().encode(task).length > 4000) return fail("too_long", "4000 bytes at most");
  const id = req.job_id;
  const now = () => new Date().toISOString();
  const job: StartJob = { id, by: "fixture@example.test", workspace_id: req.workspace_id, kind: req.kind, name: req.name, state: "running", began: now() };
  starts = [...starts, job].slice(-20);
  send(ws, { type: "ack", id: req.id, ok: true });
  publishStarts();
  const wsNum = req.workspace_id.slice(1);
  const pane_id = `${req.workspace_id}:p${paneSeq++}`;
  const step = 1200;
  setTimeout(() => {
    if (req.workspace_id === "w4") return setJob(id, { state: "failed", failed_step: "opened", error: "workspace_not_found", ended_at: now() });
    setJob(id, { opened_at: now(), pane_id, tab_id: `w${wsNum}:t${paneSeq}`, cwd: PATHS[req.workspace_id] || "~/code/web" });
  }, step);
  setTimeout(() => {
    if (req.workspace_id === "w4") return;
    if (req.kind === "codex") return setJob(id, { state: "failed", failed_step: "started", error: "timeout", ended_at: now() });
    const label = ["api", "infra", "web", "docs"][Number(wsNum) - 1]!;
    const asks = /trust/i.test(task);
    const a = agent(pane_id, req.name, asks ? "blocked" : "idle", asks ? "Waiting for you" : "Waiting for your next task", label, 0, req.kind);
    agents.set(pane_id, { ...a, changed_at: now() });
    detail(pane_id, {});
    publishPane(pane_id);
    if (asks) return setJob(id, { state: "asks_first", started_at: now(), ended_at: now() });
    setJob(id, { started_at: now() });
    setTimeout(() => {
      setJob(id, { sent_at: now(), state: "done", ended_at: now() });
      setStatus(pane_id, "working", "Thinking");
    }, step);
  }, step * 2);
}

setInterval(() => {
  if (agents.get("w1:p2")?.status !== "working" || tests >= 140) return;
  tests = Math.min(140, tests + 3);
  details.set("w1:p2", { ...details.get("w1:p2")!, steps: rateLimitSteps() });
  for (const [ws, watched] of watchers) {
    if (!watched.has("w1:p2")) continue;
    send(ws, details.get("w1:p2")!);
    send(ws, { type: "output", pane_id: "w1:p2", text: screen("w1:p2") });
  }
}, 2000);

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: PORT,
  async fetch(req, srv) {
    const url = new URL(req.url);
    if (url.pathname === "/ws") return srv.upgrade(req) ? undefined : new Response("expected a WebSocket", { status: 400 });
    const path = url.pathname === "/" ? "/index.html" : url.pathname;
    const file = Bun.file(join(DIST, path));
    if (path.includes("..") || !(await file.exists())) return new Response("not found", { status: 404 });
    return new Response(file, { headers: { "Content-Security-Policy": CSP, "X-Frame-Options": "DENY" } });
  },
  websocket: {
    open(ws) {
      watchers.set(ws, new Set());
      const snapshot: Snapshot = {
        type: "snapshot",
        host: "studio",
        role: ROLE,
        expiry: EXPIRY,
        push: { vapid_public_key: VAPID_KEY },
        agents: [...agents.values()],
        queue,
        workspaces: workspaceRows(),
        start_kinds: START_KINDS,
        starts,
      };
      send(ws, snapshot);
    },
    message(ws, raw) {
      const msg = JSON.parse(String(raw)) as Watch | ClientRequest;
      if (msg.type === "terminal") return; // the mock always sends the screen
      if (msg.type === "watch" || msg.type === "unwatch") {
        const watched = watchers.get(ws)!;
        if (msg.type === "unwatch") return void watched.delete(msg.pane_id);
        if (!agents.has(msg.pane_id)) return;
        watched.add(msg.pane_id);
        send(ws, details.get(msg.pane_id)!);
        send(ws, { type: "output", pane_id: msg.pane_id, text: screen(msg.pane_id) });
        return;
      }
      console.log("request", JSON.stringify(msg));
      if (msg.type.startsWith("push.")) return handlePush(ws, msg as PushRequest);
      if (msg.type.startsWith("queue.")) return handleQueue(ws, msg as QueueRequest);
      if (msg.type === "start") return handleStart(ws, msg);
      const r = handle(msg as PaneRequest);
      send(ws, r.ok ? { type: "ack", id: msg.id, ok: true } : { type: "ack", id: msg.id, ok: false, error: { code: r.code, message: r.message } });
    },
    close(ws) {
      watchers.delete(ws);
    },
  },
});

console.log(`mock herdr-pocket server on http://${server.hostname}:${server.port} (ws /ws)${READ_ONLY ? ", read-only" : ""}`);
