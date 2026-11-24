import type { AckErrorCode, ClientMessage, PushSettings, RequestBody, ServerMessage, Watch } from "./protocol";

/**
 * The link to the server, as the app shows it:
 * - connecting: opening a socket (also after a drop, until a frame arrives);
 * - live: a socket is open and answering;
 * - slow: open, but the liveness probe is overdue;
 * - down: no socket; the next try is scheduled (retryAt);
 * - offline: the phone says it has no network; nothing is tried until it does.
 *
 * While the page is hidden nothing is tried either: a phone cuts a hidden
 * tab's sockets off about 5 s after it goes to the background (the drop
 * reports from 2026-11-22/23: every reset came while hidden), so a socket
 * opened then dies within seconds. resume() reconnects when it is back.
 */
export type LinkState = "connecting" | "live" | "slow" | "down" | "offline";

export interface Link {
  state: LinkState;
  /** When the next try starts, while down. */
  retryAt: number | null;
}

/**
 * The probe: a ping request every probeEvery while the page is visible. A
 * phone's socket can die without a close (sleep, a network change, a NAT
 * timeout) and the browser may not notice for minutes, so a missing ack is
 * the only early sign. No ack after slowAfter: slow. None after deadAfter:
 * the socket is dropped and a new one opened at once. Any frame counts as an
 * ack.
 */
export interface Timing {
  probeEvery: number;
  slowAfter: number;
  deadAfter: number;
  /** A resume probe's deadline: after a trip away, a dead socket is likely. */
  resumeDeadAfter: number;
  /** A socket still connecting after this is given up and tried again. */
  openTimeout: number;
  /** Retries after a failed open: minDelay doubling to maxDelay. */
  minDelay: number;
  maxDelay: number;
  /** Back in view after being hidden longer than this: open a fresh socket rather than probe the old one. */
  staleAfterHidden: number;
  /**
   * After resume(), a link that isn't live is shown only once it has stayed
   * so this long, so a quick reconnect on return doesn't flash a banner.
   * While hidden, changes aren't shown at all (nobody can see them).
   */
  quietAfterResume: number;
}

export const TIMING: Timing = {
  probeEvery: 5000,
  slowAfter: 2000,
  // Generous: a phone's radio waking from a doze can take seconds to answer,
  // and each false alarm costs a fresh socket and snapshot.
  deadAfter: 12000,
  resumeDeadAfter: 2500,
  openTimeout: 6000,
  minDelay: 500,
  maxDelay: 4000,
  staleAfterHidden: 15000,
  quietAfterResume: 1000,
};

const REQUEST_TIMEOUT = 15000;

/** The close code the app sends when it gives up on a socket itself; the reason says why. */
export const GAVE_UP = 4000;

/**
 * How the last open socket ended, sent with the next try (the drop query
 * parameter) so the server can log it beside its own view of the close. It
 * carries no prompt or output text: the reason is the close frame's, or one
 * of the app's own fixed strings.
 */
export interface DropReport {
  /** The close event's code, or GAVE_UP when the app dropped the socket. */
  code: number;
  reason: string;
  /** The close handshake completed (false for a reset or a give-up). */
  clean: boolean;
  /** The page was in view when it ended. */
  visible: boolean;
  /** Since the page was last shown or hidden (or since connect, if neither). */
  since_visibility_ms: number;
  /** navigator.onLine at the drop. */
  online: boolean;
  /** navigator.connection's type and effectiveType, where the browser has them (Chrome on Android). */
  net?: string;
  effective?: string;
  /** From open to the drop. */
  lived_ms: number;
  /** Tries that failed since the drop, before this one. */
  failed_tries: number;
  /** From the drop to this try; set when sent. */
  ago_ms?: number;
}

interface NetworkInformation {
  type?: string;
  effectiveType?: string;
}

/**
 * A server error, or a client-side one: "disconnected" means it never left
 * the phone; "lost" means the socket dropped after it was sent, so it may
 * have arrived; "timeout" means no ack came back in time.
 */
export interface RequestError {
  code: AckErrorCode | "disconnected" | "lost" | "timeout";
  message: string;
}

/** settings is set only by the push.subscribe and push.get acks; itemId only by a queue.add ack. */
export type RequestResult = { ok: true; settings?: PushSettings; itemId?: string } | { ok: false; error: RequestError };

export interface Socket {
  /** Sends watch, unwatch and terminal; dropped while disconnected (the app re-sends them on the next snapshot). */
  watch(msg: Watch): void;
  /** Sends a request with a fresh id and resolves with its ack. Never rejects. */
  request(body: RequestBody): Promise<RequestResult>;
  /**
   * The page is back in view, the network is back, or the person asked to
   * retry: connect now if down or still trying; after a long time hidden,
   * start a fresh socket; else probe the open one now.
   */
  resume(hiddenFor?: number): void;
  /** The person tapped Retry: try now, even if the phone says it is offline (it can be wrong). */
  retry(): void;
  /** The phone says it is offline: drop the socket and wait for resume(). */
  offline(): void;
  close(): void;
}

export interface Hooks {
  /** A message other than an ack. */
  onMessage(msg: ServerMessage): void;
  /** The socket closed or was given up; the app's data is as of the last message. */
  onDown(): void;
  /** The link changed. */
  onLink?(link: Link): void;
}

/** The page is in view, so probing is worth it (hidden pages have throttled timers). */
function visible(): boolean {
  return typeof document === "undefined" || document.visibilityState !== "hidden";
}

/** The phone has a network, as far as the browser knows. */
function online(): boolean {
  return typeof navigator === "undefined" || navigator.onLine !== false;
}

function defaultUrl(): URL {
  const url = new URL("/ws", location.href);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return url;
}

/**
 * Keeps a WebSocket to /ws open. The server refuses or closes the socket
 * while herdr is down, so onDown covers both outages. Acks resolve their
 * request and are not passed to onMessage; a request still pending when the
 * socket goes resolves as lost, and after 15 s as timeout.
 */
export function connect(hooks: Hooks, url: URL = defaultUrl(), timing: Timing = TIMING): Socket {
  let ws: WebSocket | undefined;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let probeTimer: ReturnType<typeof setTimeout> | undefined;
  let slowTimer: ReturnType<typeof setTimeout> | undefined;
  let deadTimer: ReturnType<typeof setTimeout> | undefined;
  let openTimer: ReturnType<typeof setTimeout> | undefined;
  let delay = timing.minDelay;
  let stopped = false;
  let link: Link = { state: "connecting", retryAt: null };
  // What the hook last saw; it lags link while hidden or just after a resume.
  let shown: Link = link;
  let quietUntil = 0;
  let quietTimer: ReturnType<typeof setTimeout> | undefined;
  // Tries since the last frame: after one has failed, a new try shows as down
  // (can't reach it, trying again), not as a plain connecting.
  let failures = 0;
  let nextId = 1;
  const pending = new Map<string, (result: RequestResult) => void>();
  // The drop diagnostics: when the open socket opened, when the page was last
  // shown or hidden, and the last drop not yet delivered to the server.
  let openedAt: number | null = null;
  let visibilityChangedAt = Date.now();
  const onVisibility = () => (visibilityChangedAt = Date.now());
  if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility);
  let drop: { report: DropReport; at: number } | null = null;

  // Notes how the open socket ended; a try that never opened only counts against the last drop.
  function recordDrop(code: number, reason: string, clean: boolean) {
    if (openedAt === null) {
      if (drop) drop.report.failed_tries++;
      return;
    }
    const now = Date.now();
    const conn = typeof navigator === "undefined" ? undefined : (navigator as { connection?: NetworkInformation }).connection;
    drop = {
      at: now,
      report: {
        code,
        reason,
        clean,
        visible: visible(),
        since_visibility_ms: now - visibilityChangedAt,
        online: online(),
        ...(conn?.type ? { net: conn.type } : {}),
        ...(conn?.effectiveType ? { effective: conn.effectiveType } : {}),
        lived_ms: now - openedAt,
        failed_tries: 0,
      },
    };
    openedAt = null;
  }

  function dialUrl(): URL {
    if (!drop) return url;
    const u = new URL(url);
    u.searchParams.set("drop", JSON.stringify({ ...drop.report, ago_ms: Date.now() - drop.at }));
    return u;
  }

  function setLink(state: LinkState, retryAt: number | null = null) {
    if (link.state !== state || link.retryAt !== retryAt) link = { state, retryAt };
    show();
  }

  // Passes the link to the hook, unless it isn't live and the page is hidden
  // or was resumed less than quietAfterResume ago (a timer shows it then).
  function show() {
    if (link.state !== "live" && (!visible() || Date.now() < quietUntil)) return;
    if (shown.state === link.state && shown.retryAt === link.retryAt) return;
    shown = link;
    hooks.onLink?.(link);
  }

  function failAll(code: RequestError["code"], message: string) {
    for (const resolve of pending.values()) resolve({ ok: false, error: { code, message } });
    pending.clear();
  }

  function send(msg: ClientMessage | { type: "ping"; id: string }): boolean {
    if (ws?.readyState !== WebSocket.OPEN) return false;
    ws.send(JSON.stringify(msg));
    return true;
  }

  function clearProbe() {
    clearTimeout(probeTimer);
    clearTimeout(slowTimer);
    clearTimeout(deadTimer);
  }

  function scheduleProbe() {
    clearTimeout(probeTimer);
    probeTimer = setTimeout(() => {
      // Hidden: skip it (timers are throttled, so it would misfire); resume() checks on return.
      if (visible()) probe(timing.deadAfter);
      else scheduleProbe();
    }, timing.probeEvery);
  }

  function probe(deadAfter: number) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    clearProbe();
    // Probe ids start with "p", requests' with "r".
    const id = `p${nextId++}`;
    slowTimer = setTimeout(() => setLink("slow"), Math.min(timing.slowAfter, deadAfter));
    deadTimer = setTimeout(() => replace("no reply to the probe"), deadAfter);
    send({ type: "ping", id });
  }

  // Lets go of the current socket without waiting for its close (which a
  // half-open socket may never deliver), failing what was in flight.
  function abandon(why: string) {
    const old = ws;
    ws = undefined;
    if (old) old.onopen = old.onmessage = old.onclose = old.onerror = null;
    // From onclose the socket is already closed, and its drop recorded.
    if (old && old.readyState !== WebSocket.CLOSED) {
      // A code and reason, so the server's log says the app gave up and why
      // (a bare close() reaches it as "no status received").
      old.close(GAVE_UP, why);
      recordDrop(GAVE_UP, why, false);
    }
    clearProbe();
    clearTimeout(openTimer);
    clearTimeout(retry);
    failAll("lost", why);
    if (old) hooks.onDown();
  }

  // Gives up on the current socket and opens a new one at once.
  function replace(why: string) {
    failures++;
    abandon(why);
    if (!stopped) open();
  }

  function open(force = false) {
    clearTimeout(retry);
    if (!force && !online()) {
      setLink("offline");
      return;
    }
    if (!force && !visible()) {
      // A socket opened now would be cut off within seconds; resume() opens it.
      setLink("down");
      return;
    }
    setLink(failures > 0 ? "down" : "connecting");
    const sock = new WebSocket(dialUrl());
    ws = sock;
    openTimer = setTimeout(() => {
      if (sock.readyState === WebSocket.CONNECTING) replace("connecting took too long");
    }, timing.openTimeout);
    sock.onopen = () => {
      clearTimeout(openTimer);
      openedAt = Date.now();
      scheduleProbe();
    };
    sock.onmessage = (e) => {
      delay = timing.minDelay;
      failures = 0;
      // The server has the report (it came with this socket's request).
      drop = null;
      // Any frame proves the socket alive: the probe in flight is settled.
      clearProbe();
      scheduleProbe();
      setLink("live");
      let msg: ServerMessage;
      try {
        msg = JSON.parse(e.data) as ServerMessage;
      } catch (err) {
        console.error("herdr-pocket: bad message", err);
        return;
      }
      if (msg.type === "ack") {
        if (msg.id.startsWith("p")) return; // a probe's ack; handled above
        const result: RequestResult = !msg.ok
          ? { ok: false, error: msg.error }
          : {
              ok: true,
              ...(msg.settings ? { settings: msg.settings } : {}),
              ...(msg.item_id ? { itemId: msg.item_id } : {}),
            };
        pending.get(msg.id)?.(result);
        pending.delete(msg.id);
        return;
      }
      hooks.onMessage(msg);
    };
    sock.onclose = (e) => {
      if (ws !== sock) return;
      recordDrop(e.code, e.reason, e.wasClean);
      abandon("the connection closed");
      if (stopped) return;
      if (!online()) {
        setLink("offline");
        return;
      }
      failures++;
      setLink("down", Date.now() + delay);
      retry = setTimeout(open, delay);
      delay = Math.min(delay * 2, timing.maxDelay);
    };
  }

  open();
  return {
    watch: (msg) => void send(msg),
    request(body) {
      const id = `r${nextId++}`;
      return new Promise((resolve) => {
        if (!send({ ...body, id } as ClientMessage)) {
          resolve({ ok: false, error: { code: "disconnected", message: "not connected" } });
          return;
        }
        const t = setTimeout(() => {
          pending.delete(id);
          resolve({ ok: false, error: { code: "timeout", message: "no ack" } });
        }, REQUEST_TIMEOUT);
        pending.set(id, (result) => {
          clearTimeout(t);
          resolve(result);
        });
      });
    },
    resume(hiddenFor = 0) {
      if (stopped) return;
      delay = timing.minDelay;
      // A drop while hidden isn't a try that failed: the first try back shows as connecting.
      failures = 0;
      quietUntil = Date.now() + timing.quietAfterResume;
      clearTimeout(quietTimer);
      // Ends the quiet itself: a timer can fire a little before Date.now() reaches quietUntil.
      quietTimer = setTimeout(() => {
        quietUntil = 0;
        show();
      }, timing.quietAfterResume);
      // Not a failure: a resume starts over, it doesn't count as a try that failed.
      if (!ws || ws.readyState !== WebSocket.OPEN) {
        // Down, offline, or a try that may have stalled while hidden: start over now.
        abandon("resumed");
        open();
      } else if (hiddenFor > timing.staleAfterHidden) {
        // A socket that sat through a long sleep is more likely dead than
        // not, and a fresh one brings a fresh snapshot.
        abandon("back after a long time hidden");
        open();
      } else {
        probe(timing.resumeDeadAfter);
      }
    },
    retry() {
      if (stopped) return;
      delay = timing.minDelay;
      abandon("retry");
      open(true);
    },
    offline() {
      if (stopped) return;
      abandon("the phone went offline");
      setLink("offline");
    },
    close() {
      stopped = true;
      clearTimeout(quietTimer);
      if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibility);
      abandon("closed");
    },
  };
}
