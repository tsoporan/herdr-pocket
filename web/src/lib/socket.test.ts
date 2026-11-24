import { afterAll, expect, test } from "bun:test";
import type { ServerMessage } from "./protocol";
import { connect, type DropReport, GAVE_UP, type Link, TIMING, type Timing } from "./socket";

// A real Bun WebSocket server on a free port; it acks "keys", refuses "prompt" and acks "push.get" with settings.
const SETTINGS = { blocked: true, done: false, quiet: null, desk_minutes: 5 };
// It acks pings unless the URL says ?silent, which plays a socket that died without a close.
// Counts per path: opens and pings seen.
const seen = new Map<string, { opens: number; pings: number }>();
const count = (path: string) => seen.get(path) ?? seen.set(path, { opens: 0, pings: 0 }).get(path)!;
// Each open's drop report, per path (null when the try carried none).
const drops = new Map<string, (DropReport | null)[]>();
// /closer's, /hideretry's and /dropone's server close the first socket on them, with a code and reason;
// /closeall's closes every socket after 30 ms.
const server = Bun.serve<{ silent: boolean; path: string }>({
  hostname: "127.0.0.1",
  port: 0,
  fetch(req, srv) {
    const u = new URL(req.url);
    const drop = u.searchParams.get("drop");
    drops.set(u.pathname, [...(drops.get(u.pathname) ?? []), drop === null ? null : (JSON.parse(drop) as DropReport)]);
    return srv.upgrade(req, { data: { silent: u.searchParams.has("silent"), path: u.pathname } })
      ? undefined
      : new Response("no", { status: 400 });
  },
  websocket: {
    open(ws) {
      count(ws.data.path).opens++;
      ws.send(JSON.stringify({ type: "snapshot", host: "test", agents: [], workspaces: [] }));
      if (ws.data.path === "/closeall") setTimeout(() => ws.close(4100, "test close"), 30);
      if (["/closer", "/hideretry", "/dropone"].includes(ws.data.path) && count(ws.data.path).opens === 1) setTimeout(() => ws.close(4100, "test close"), 30);
      if (ws.data.path === "/chatty") {
        const t = setInterval(() => {
          if (ws.readyState !== 1) return clearInterval(t);
          ws.send(JSON.stringify({ type: "patch", agents: [] }));
        }, 20);
      }
    },
    message(ws, raw) {
      const msg = JSON.parse(String(raw));
      if (msg.type === "ping") {
        count(ws.data.path).pings++;
        if (!ws.data.silent) ws.send(JSON.stringify({ type: "ack", id: msg.id, ok: true }));
      }
      if (msg.type === "keys") ws.send(JSON.stringify({ type: "ack", id: msg.id, ok: true }));
      if (msg.type === "prompt")
        ws.send(
          JSON.stringify({ type: "ack", id: msg.id, ok: false, error: { code: "agent_blocked", message: "blocked" } }),
        );
      if (msg.type === "push.get") ws.send(JSON.stringify({ type: "ack", id: msg.id, ok: true, settings: SETTINGS }));
      if (msg.type === "text") ws.close();
    },
  },
});
afterAll(() => server.stop(true));

function open() {
  const messages: ServerMessage[] = [];
  let ready!: () => void;
  const snapshot = new Promise<void>((r) => (ready = r));
  const socket = connect(
    {
      onMessage(m) {
        messages.push(m);
        if (m.type === "snapshot") ready();
      },
      onDown() {},
    },
    new URL(`ws://127.0.0.1:${server.port}/ws`),
  );
  return { socket, messages, snapshot };
}

test("requests resolve with their own ack, which onMessage never sees", async () => {
  const { socket, messages, snapshot } = open();
  await snapshot;
  const [a, b] = await Promise.all([
    socket.request({ type: "keys", pane_id: "w1:p1", keys: ["esc"] }),
    socket.request({ type: "prompt", pane_id: "w1:p1", text: "hi" }),
  ]);
  expect(a).toEqual({ ok: true });
  expect(b).toEqual({ ok: false, error: { code: "agent_blocked", message: "blocked" } });
  expect(messages.map((m) => m.type)).toEqual(["snapshot"]);
  socket.close();
});

test("an ack's settings reach the request", async () => {
  const { socket, snapshot } = open();
  await snapshot;
  expect(await socket.request({ type: "push.get", endpoint: "https://fcm.googleapis.com/fcm/send/x" })).toEqual({
    ok: true,
    settings: SETTINGS,
  });
  socket.close();
});

test("a request pending when the socket closes resolves as lost: it may have arrived", async () => {
  const { socket, snapshot } = open();
  await snapshot;
  const r = await socket.request({ type: "text", pane_id: "w1:p1", text: "x" });
  expect(r).toEqual({ ok: false, error: { code: "lost", message: "the connection closed" } });
  socket.close();
});

test("a request before the socket opens fails at once", async () => {
  const { socket } = open();
  expect(await socket.request({ type: "keys", pane_id: "w1:p1", keys: ["esc"] })).toEqual({
    ok: false,
    error: { code: "disconnected", message: "not connected" },
  });
  socket.close();
});

const FAST: Timing = {
  probeEvery: 60,
  slowAfter: 40,
  deadAfter: 120,
  resumeDeadAfter: 30,
  openTimeout: 1000,
  minDelay: 300,
  maxDelay: 600,
  staleAfterHidden: 10000,
  quietAfterResume: 0,
};
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// Records link states (deduplicated runs) and downs.
function probed(path: string, silent: boolean, timing: Timing = FAST) {
  const events: string[] = [];
  const push = (e: string) => void (events.at(-1) !== e && events.push(e));
  const url = new URL(`ws://127.0.0.1:${server.port}${path}${silent ? "?silent" : ""}`);
  const socket = connect(
    { onMessage() {}, onDown: () => push("down"), onLink: (l) => push(l.state) },
    url,
    timing,
  );
  return { socket, events };
}

test("an answered probe keeps the socket: live, never slow, never reopened", async () => {
  const { socket, events } = probed("/alive", false);
  await sleep(400);
  socket.close();
  expect(count("/alive").pings).toBeGreaterThanOrEqual(3);
  expect(count("/alive").opens).toBe(1);
  expect(events).toEqual(["live", "down"]); // the down is close()
});

test("an unanswered probe says slow, then replaces the socket at once", async () => {
  const { socket, events } = probed("/silent", true);
  // first frame live; probe at 60, slow at 100, replaced at 180 without backoff.
  await sleep(260);
  socket.close();
  // The new try shows as down (it couldn't reach it), until its first frame.
  expect(events.slice(0, 4)).toEqual(["live", "slow", "down", "live"]);
  expect(count("/silent").opens).toBeGreaterThanOrEqual(2);
});

test("a request in flight when the probe gives up resolves as lost", async () => {
  const { socket } = probed("/lost", true);
  await sleep(20);
  // "rename" gets no ack from this server, so it is still pending at the drop.
  const r = await socket.request({ type: "rename", pane_id: "w1:p1", name: "x" });
  socket.close();
  expect(r).toEqual({ ok: false, error: { code: "lost", message: "no reply to the probe" } });
});

test("resume probes an open socket at once, with the shorter deadline", async () => {
  const { socket, events } = probed("/sleepy", true, { ...FAST, probeEvery: 10000, slowAfter: 5000, deadAfter: 5000 });
  await sleep(50);
  expect(count("/sleepy").pings).toBe(0);
  socket.resume(1000);
  await sleep(100);
  socket.close();
  expect(count("/sleepy").pings).toBe(1);
  expect(count("/sleepy").opens).toBe(2); // replaced 30 ms after the resume probe
  expect(events.slice(0, 4)).toEqual(["live", "slow", "down", "live"]);
});

test("resume after a long time hidden opens a fresh socket without probing, and isn't called a failure", async () => {
  const { socket, events } = probed("/longaway", false);
  await sleep(50);
  socket.resume(60000);
  await sleep(50);
  socket.close();
  expect(count("/longaway").opens).toBe(2);
  expect(events).toEqual(["live", "down", "connecting", "live", "down"]);
});

test("any frame counts as alive: an unanswered probe isn't dropped while frames arrive", async () => {
  const { socket, events } = probed("/chatty", true);
  await sleep(400);
  socket.close();
  expect(events).toEqual(["live", "down"]);
});

test("a close says down with when it tries again, and resume skips the wait", async () => {
  let ready!: () => void;
  const first = new Promise<void>((r) => (ready = r));
  const links: Link[] = [];
  const socket = connect(
    { onMessage: (m) => void (m.type === "snapshot" && ready()), onDown() {}, onLink: (l) => void links.push(l) },
    new URL(`ws://127.0.0.1:${server.port}/back`),
    { ...FAST, minDelay: 5000, maxDelay: 5000 },
  );
  await first;
  // The server closes the socket on "text".
  void socket.request({ type: "text", pane_id: "w1:p1", text: "x" });
  await sleep(50);
  const down = links.at(-1)!;
  expect(down.state).toBe("down");
  expect(down.retryAt! - Date.now()).toBeGreaterThan(4000);
  socket.resume();
  await sleep(100);
  socket.close();
  expect(count("/back").opens).toBe(2);
});

test("offline drops the socket and tries nothing until resume", async () => {
  const { socket, events } = probed("/offline", false);
  await sleep(50);
  socket.offline();
  await sleep(200);
  expect(count("/offline").opens).toBe(1);
  expect(events.at(-1)).toBe("offline");
  socket.resume();
  await sleep(100);
  socket.close();
  expect(count("/offline").opens).toBe(2);
});

test("retry tries even while the phone says it is offline", async () => {
  const { socket, events } = probed("/forced", false);
  await sleep(50);
  socket.offline();
  socket.retry();
  await sleep(100);
  socket.close();
  expect(count("/forced").opens).toBe(2);
  expect(events).toContain("offline");
  expect(events.at(-2)).toBe("live");
});

test("the probe allows a slow reply 10 to 15 s before it drops the socket", () => {
  expect(TIMING.deadAfter).toBeGreaterThanOrEqual(10000);
  expect(TIMING.deadAfter).toBeLessThanOrEqual(15000);
  expect(TIMING.slowAfter).toBeLessThan(TIMING.deadAfter);
});

test("the next try reports how the server closed the last socket, once", async () => {
  const { socket } = probed("/closer", false, { ...FAST, minDelay: 50 });
  await sleep(200);
  socket.close();
  const [first, second] = drops.get("/closer")!;
  expect(first).toBeNull();
  expect(second).toMatchObject({ code: 4100, reason: "test close", clean: true, visible: true, online: true, failed_tries: 0 });
  expect(second!.lived_ms).toBeGreaterThanOrEqual(20);
  expect(second!.ago_ms).toBeGreaterThanOrEqual(40);
  expect(drops.get("/closer")!.length).toBe(2);
});

test("a socket the probe gave up on is reported with the app's own code and why", async () => {
  const { socket } = probed("/gaveup", true);
  await sleep(260);
  socket.close();
  const reports = drops.get("/gaveup")!;
  expect(reports[0]).toBeNull();
  expect(reports[1]).toMatchObject({ code: GAVE_UP, reason: "no reply to the probe", clean: false });
});

// Plays the page as hidden (bun has no document) until the returned function shows it again.
function hidePage(): () => void {
  const g = globalThis as { document?: unknown };
  g.document = { visibilityState: "hidden", addEventListener() {}, removeEventListener() {} };
  return () => delete g.document;
}

// Records what reached onLink, apart from the downs.
function tracked(path: string, silent: boolean, timing: Timing) {
  const links: string[] = [];
  let downs = 0;
  const url = new URL(`ws://127.0.0.1:${server.port}${path}${silent ? "?silent" : ""}`);
  const socket = connect({ onMessage() {}, onDown: () => downs++, onLink: (l) => links.push(l.state) }, url, timing);
  return { socket, links, downs: () => downs };
}

test("a drop while hidden isn't retried, nor shown; resume reconnects as connecting", async () => {
  const { socket, links } = tracked("/closeall", false, { ...FAST, minDelay: 20, quietAfterResume: 0 });
  try {
    await sleep(10);
    expect(links).toEqual(["live"]);
    const show = hidePage();
    try {
      await sleep(200); // closed at 30 ms; a retry would have come at 50
      expect(count("/closeall").opens).toBe(1);
      expect(links).toEqual(["live"]);
    } finally {
      show();
    }
    socket.resume(200);
    await sleep(25);
    expect(count("/closeall").opens).toBe(2);
    expect(links.slice(0, 3)).toEqual(["live", "connecting", "live"]);
  } finally {
    socket.close();
  }
});

test("a retry due after the page hides waits for resume", async () => {
  const { socket, links } = tracked("/hideretry", false, { ...FAST, minDelay: 150, quietAfterResume: 0 });
  let show = () => {};
  try {
    await sleep(60);
    expect(links).toEqual(["live", "down"]); // dropped in view: shown, retry in 150 ms
    show = hidePage();
    await sleep(200); // the retry fired while hidden
    expect(count("/hideretry").opens).toBe(1);
    show();
    socket.resume(200);
    await sleep(25);
    expect(count("/hideretry").opens).toBe(2);
  } finally {
    show();
    socket.close();
  }
});

test("connect while hidden opens nothing until resume", async () => {
  const show = hidePage();
  let socket;
  try {
    socket = tracked("/bornhidden", false, FAST).socket;
    await sleep(50);
    expect(count("/bornhidden").opens).toBe(0);
  } finally {
    show();
  }
  socket.resume();
  await sleep(25);
  socket.close();
  expect(count("/bornhidden").opens).toBe(1);
});

test("after resume, a quick reconnect shows no banner", async () => {
  const { socket, links } = tracked("/quick", false, { ...FAST, quietAfterResume: 300 });
  await sleep(50);
  socket.resume(60000); // a fresh socket: connecting, then live within ms
  await sleep(100);
  socket.close();
  expect(count("/quick").opens).toBe(2);
  expect(links).toEqual(["live"]);
});

test("during the quiet, live is shown at once", async () => {
  const { socket, links } = tracked("/dropone", false, { ...FAST, minDelay: 1000, quietAfterResume: 300 });
  try {
    await sleep(60); // closed in view at 30 ms: down is shown
    expect(links).toEqual(["live", "down"]);
    socket.resume(200); // quiet until +300; the new socket goes live within ms
    await sleep(50);
    expect(links).toEqual(["live", "down", "live"]);
  } finally {
    socket.close();
  }
});

test("after resume, a link still not live when the quiet ends is shown", async () => {
  const { socket, links } = tracked("/stuck", true, { ...FAST, probeEvery: 10000, resumeDeadAfter: 1000, slowAfter: 50, quietAfterResume: 150 });
  await sleep(50);
  socket.resume(1000); // probes; slow at 50 ms, held back until 150
  await sleep(100);
  expect(links).toEqual(["live"]);
  await sleep(100);
  socket.close();
  expect(links).toEqual(["live", "slow"]);
});
