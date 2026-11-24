// What the service worker does with a push and with a tap on its
// notification. Pure functions; sw.ts does the calls. Only sw.ts imports this
// module, so the service worker bundles it and has no imports of its own.

import type { AnswerRequest, PushAnswer, PushPayload } from "./protocol";

/** What the notification keeps for the app and for a tap. */
export interface NotificationData {
  url: string;
  kind: PushPayload["kind"];
  // With buttons: what they send, and the agent's title for the outcome.
  answer?: PushAnswer;
  name?: string;
  // An answer that was sent: the clear that follows (the agent left blocked) leaves it up.
  sent?: true;
}

/** A notification button; not in TypeScript's NotificationOptions. */
export interface NotificationButton {
  action: AnswerChoice;
  title: string;
}

export type AnswerChoice = "allow" | "deny";

export type ShowOptions = NotificationOptions & { renotify?: boolean; actions?: NotificationButton[] };

/** A notification to show. */
export interface Notice {
  title: string;
  options: ShowOptions;
}

export type PushAction = { do: "show"; title: string; options: ShowOptions } | { do: "clear"; tag: string };

const KINDS = new Set<string>(["blocked", "done", "clear", "expiry", "test"]);

const ICON = "/icon-192.png";
const BADGE = "/icon-monochrome-512.png";

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

const MAX_ID = 256;
const DIGIT = /^[1-9]$/;

function id(v: unknown): v is string {
  return typeof v === "string" && v.length > 0 && v.length <= MAX_ID;
}

/** The answer object, or undefined when it isn't one the server sends. */
function parseAnswer(v: unknown): PushAnswer | undefined {
  if (typeof v !== "object" || v === null) return undefined;
  const o = v as Record<string, unknown>;
  if (!id(o.pane_id) || !id(o.question_id)) return undefined;
  if (typeof o.allow !== "string" || typeof o.deny !== "string") return undefined;
  if (!DIGIT.test(o.allow) || !DIGIT.test(o.deny) || o.allow === o.deny) return undefined;
  return { pane_id: o.pane_id, question_id: o.question_id, allow: o.allow, deny: o.deny };
}

/** The payload, or null when it isn't one the server sends. */
export function parsePayload(text: string | undefined): PushPayload | null {
  if (!text) return null;
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof v !== "object" || v === null) return null;
  const o = v as Record<string, unknown>;
  if (typeof o.kind !== "string" || !KINDS.has(o.kind)) return null;
  const answer = o.kind === "blocked" ? parseAnswer(o.answer) : undefined;
  return {
    kind: o.kind as PushPayload["kind"],
    tag: str(o.tag),
    title: str(o.title),
    body: str(o.body),
    url: str(o.url),
    ...(answer && { answer }),
  };
}

/**
 * A clear closes that tag's notifications and shows nothing. Anything else
 * shows one; a payload that can't be read still shows a notification, since
 * every push must show one (userVisibleOnly).
 */
export function pushAction(payload: PushPayload | null): PushAction {
  if (payload?.kind === "clear" && payload.tag) return { do: "clear", tag: payload.tag };
  if (!payload || payload.kind === "clear") {
    const data: NotificationData = { url: "/", kind: "test" };
    return { do: "show", title: "herdr-pocket", options: { body: "Open the app to see what changed.", icon: ICON, badge: BADGE, data } };
  }
  const title = payload.title || "herdr-pocket";
  const answer = payload.kind === "blocked" ? payload.answer : undefined;
  const data: NotificationData = { url: payload.url || "/", kind: payload.kind, ...(answer && { answer, name: title }) };
  return {
    do: "show",
    title,
    options: {
      body: payload.body,
      // An empty tag would make every untagged alert one notification; leave it out instead.
      ...(payload.tag && { tag: payload.tag }),
      // A newer blocked alert for the same agent buzzes again; other kinds replace quietly.
      renotify: payload.kind === "blocked" && Boolean(payload.tag),
      icon: ICON,
      badge: BADGE,
      data,
      ...(answer && { actions: BUTTONS }),
    },
  };
}

const BUTTONS: NotificationButton[] = [
  { action: "allow", title: "Allow once" },
  { action: "deny", title: "Don’t allow" },
];

/**
 * What a button tap sends, or null when the tap isn't a button or the
 * notification has no answer (a plain tap opens the agent instead).
 */
export function answerRequest(action: string, data: unknown): { choice: AnswerChoice; body: AnswerRequest } | null {
  if (action !== "allow" && action !== "deny") return null;
  if (typeof data !== "object" || data === null) return null;
  const answer = parseAnswer((data as Record<string, unknown>).answer);
  if (!answer) return null;
  return { choice: action, body: { pane_id: answer.pane_id, question_id: answer.question_id, option: answer[action] } };
}

/**
 * POST /api/answer's outcome: "ok", the error code, or null when the server
 * couldn't be reached or didn't answer in JSON.
 */
export type AnswerOutcome = string | null;

/** The outcome of a parsed /api/answer response body. */
export function answerOutcome(body: unknown): AnswerOutcome {
  if (typeof body !== "object" || body === null) return null;
  const o = body as Record<string, unknown>;
  if (o.ok === true) return "ok";
  if (o.ok === false && typeof o.code === "string" && o.code) return o.code;
  return null;
}

/** Shown under the same tag while the answer is on its way (the mockup's sending state). */
export function sendingNotice(data: unknown, tag: string): Notice {
  const d = (typeof data === "object" && data !== null ? data : {}) as Record<string, unknown>;
  const next: NotificationData = { url: str(d.url) || "/", kind: "blocked" };
  return {
    title: str(d.name) || "herdr-pocket",
    options: { body: "Checking the screen, then sending…", ...(tag && { tag }), renotify: false, icon: ICON, badge: BADGE, data: next },
  };
}

/** Whether a clear for its tag closes this notification: all but a sent answer's outcome. */
export function closesOnClear(data: unknown): boolean {
  return !(typeof data === "object" && data !== null && (data as Record<string, unknown>).sent === true);
}

/** The node's short name for "Couldn't reach": the hostname's first label, or a whole IP address. */
export function hostLabel(hostname: string): string {
  if (hostname.includes(":") || /^[\d.]+$/.test(hostname)) return hostname;
  return hostname.split(".")[0] || hostname;
}

/**
 * The notification that replaces a button's: same tag, no buttons, no buzz,
 * and a tap still opens the agent. Only a refusal by the server says it
 * didn't send; no reply or a herdr error says it may have gone.
 */
export function answeredAction(choice: AnswerChoice, outcome: AnswerOutcome, data: unknown, tag: string, host: string): Notice {
  const d = (typeof data === "object" && data !== null ? data : {}) as Record<string, unknown>;
  const name = str(d.name) || "The agent";
  const next: NotificationData = { url: str(d.url) || "/", kind: "blocked", ...(outcome === "ok" && { sent: true as const }) };
  let title: string;
  let body: string;
  if (outcome === "ok" && choice === "allow") {
    title = `Allowed once · ${name}`;
    body = "It’s working again.";
  } else if (outcome === "ok") {
    title = `Didn’t allow · ${name}`;
    body = "It’s waiting for your next message.";
  } else if (outcome === "question_changed") {
    title = "Didn’t send: the question changed";
    body = `${name} is asking something new. Tap to see it.`;
  } else if (outcome === null) {
    // No reply: the request may never have arrived, or the reply was lost after the digit went.
    title = `No reply from ${host}`;
    body = "It may not have been sent. Check Tailscale is on, then tap to see the agent.";
  } else if (outcome === "herdr_error") {
    title = `Not sure it was sent · ${name}`;
    body = "herdr reported an error. Tap to see the agent.";
  } else {
    // The server refused it before sending anything.
    title = `Didn’t send · ${name}`;
    body = "Tap to answer in the app.";
  }
  return {
    title,
    options: { body, ...(tag && { tag }), renotify: false, icon: ICON, badge: BADGE, data: next },
  };
}

/** The same-origin path a tap opens; anything else opens the app's root. */
export function clickTarget(data: unknown, origin: string): string {
  const url = typeof data === "object" && data !== null ? (data as Record<string, unknown>).url : undefined;
  if (typeof url !== "string" || !url) return `${origin}/`;
  try {
    const u = new URL(url, origin);
    return u.origin === origin ? u.href : `${origin}/`;
  } catch {
    return `${origin}/`;
  }
}
