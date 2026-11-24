// The plain words the agent view and the answer sheet show. Pure functions.

import type { QueueItem, QuestionOption, Status } from "./protocol";
import type { RequestError } from "./socket";

/** "under a minute", "14 min", "2 h 5 min", "3 d". */
export function durationLabel(ms: number): string {
  const min = Math.floor(Math.max(0, ms) / 60000);
  if (min < 1) return "under a minute";
  if (min < 60) return `${min} min`;
  const h = Math.floor(min / 60);
  if (h < 24) return min % 60 ? `${h} h ${min % 60} min` : `${h} h`;
  return `${Math.floor(h / 24)} d`;
}

/** How long ago the last word from the computer came, to the second while under a minute. */
export function agoLabel(at: number | null, now: number): string {
  if (at === null) return "nothing yet";
  const s = Math.max(0, Math.round((now - at) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return `${s} s ago`;
  return `${durationLabel(now - at)} ago`;
}

/**
 * The connection in words, for the Agents list's line and the agent view's
 * bar. title is the short state; detail says what that means for what is on
 * screen. A live link has no detail: nothing to warn about.
 */
export function linkWords(
  state: "connecting" | "live" | "slow" | "down" | "offline",
  retryAt: number | null,
  lastUpdate: number | null,
  now: number,
): { title: string; detail: string | null } {
  const ago = agoLabel(lastUpdate, now);
  const stale = lastUpdate === null ? null : `Last update ${ago}.`;
  switch (state) {
    case "live":
      return { title: "Connected", detail: null };
    case "slow":
      return { title: "Slow connection", detail: `Checking it… ${stale ?? ""}`.trim() };
    case "connecting":
      return { title: "Connecting…", detail: stale };
    case "down": {
      const wait = retryAt === null ? 0 : Math.ceil((retryAt - now) / 1000);
      return { title: wait > 0 ? `Can’t reach the computer · trying again in ${wait} s` : "Can’t reach the computer · trying again", detail: stale };
    }
    case "offline":
      return { title: "You’re offline", detail: stale ? `${stale} It reconnects when the network is back.` : "It connects when the network is back." };
  }
}

/** The bold part of the agent view's status line, e.g. "Working for 14 min". */
export function statusLabel(status: Status, since: string, now: number): string {
  const d = durationLabel(now - Date.parse(since));
  switch (status) {
    case "working":
      return `Working for ${d}`;
    case "blocked":
      return `Waiting for you · ${d}`;
    case "done":
      return d === "under a minute" ? "Finished just now" : `Finished ${d} ago`;
    case "idle":
      return `Idle for ${d}`;
    default:
      return "Can't tell what it's doing";
  }
}

const SENT: Record<string, string> = {
  "Send answers": "Sent your answers",
  "Cancel the question": "Cancelled the question",
  "Allow once": "Allowed once",
  "Always allow": "Always allowed",
  "Don’t allow": "Declined",
  "Don't allow": "Declined",
};

/** The confirmation title after an answer, e.g. "Allowed once". */
export function sentLabel(option: QuestionOption, note: string | undefined): string {
  const label = SENT[option.label];
  if (option.own) return "Sent your answer";
  if (note) return label === "Declined" ? "Declined, with your note" : "Sent your note";
  return label ?? `Chose “${option.label}”`;
}

/** The expiry banner's title, from the server's whole-day count. */
export function expiryTitle(daysLeft: number): string {
  if (daysLeft <= 0) return "Tailscale sign-in has ended";
  if (daysLeft === 1) return "Tailscale sign-in ends tomorrow";
  return `Tailscale sign-in ends in ${daysLeft} days`;
}

/** The expiry banner's body; host is the computer's name from the snapshot. */
export function expiryBody(daysLeft: number, host: string): string {
  if (daysLeft <= 0) return `This app can't reach ${host} until it's signed in again.`;
  return `After that this app can't reach ${host}. Turn off key expiry for herdr-pocket, or sign it in again.`;
}

/** The line under the confirmation title: a cancelled question leaves the agent waiting. */
export function sentSub(option: QuestionOption): string {
  return option.label === "Cancel the question" ? "It's waiting for a message now." : "The agent is working again.";
}

/** A failed request in plain words. */
export function errorText(error: RequestError): string {
  switch (error.code) {
    case "agent_blocked":
      return "It's waiting for an answer, so the message wasn't sent.";
    case "question_changed":
      return "The question changed on the desktop. Here's the new one.";
    case "still_blocked":
      return "It's still waiting on the desktop, so your note wasn't sent.";
    case "stopped":
      // The server's message says how far it got (the box may hold the text).
      return `Stopped: ${error.message || "the agent's screen changed, so nothing more was sent"}.`;
    case "read_only":
      return "This server is read-only, so nothing was sent.";
    case "forbidden":
      return "You can view this agent but not control it.";
    case "not_found":
      return "This agent isn't running any more.";
    case "herdr_error":
      return "herdr couldn't do that. Try again.";
    case "invalid_request":
      return "herdr-pocket couldn't send that.";
    case "queue_full":
      return "Its list is full. Remove one before lining up more.";
    case "queue_failed":
      return "The computer couldn’t save the list, so nothing changed. Try again.";
    case "not_waiting":
      return "That one has already gone, so it can’t be removed.";
    case "disconnected":
      return "Not sent: no connection right now.";
    case "lost":
      return "The connection dropped before the computer answered, so it may have gone. Check before sending again.";
    case "timeout":
      return "No reply from the computer. Check the desktop before trying again.";
    default:
      return error.message || "Something went wrong.";
  }
}

/** A Lined up item's state line. next is true for the first waiting item, the one that goes out first. */
export function lineUpLabel(item: QueueItem, next: boolean, now: number): string {
  const sent = () => {
    const d = durationLabel(now - Date.parse(item.sent_at ?? item.added));
    return d === "under a minute" ? "Sent just now" : `Sent ${d} ago`;
  };
  switch (item.state) {
    case "waiting":
      return next ? "Next, when it’s done" : "After that";
    case "sent":
      return sent();
    case "picked_up":
      return `${sent()} · it picked it up`;
    case "stalled":
      return "Sent, but it didn’t start";
    case "failed":
      return `Couldn’t send: ${refusalText(item.error)}`;
    case "dropped":
      return "Not sent: the agent closed";
  }
}

/**
 * The line under a message lined up from the field while the agent worked:
 * what happens to it now. item is undefined until the server has it; ahead
 * is how many of its items wait before it.
 */
export function followLabel(item: QueueItem | undefined, ahead: number, now: number): string {
  if (!item) return "Lining up…";
  switch (item.state) {
    case "waiting":
      return ahead > 0 ? `Lined up · goes when it finishes, after ${ahead} more` : "Lined up · goes when it finishes";
    case "sent":
      return "Sent · waiting for it to start";
    case "picked_up":
      return "✓ Sent · it picked it up";
    default:
      return lineUpLabel(item, false, now);
  }
}

/** Why herdr refused a lined-up item, from its error code. Unknown codes get general words, never the raw text. */
function refusalText(code: string | undefined): string {
  switch (code) {
    case "agent_blocked":
      return "it was waiting for an answer";
    case "agent_not_found":
    case "pane_not_found":
      return "the agent wasn’t there";
    case "forbidden":
      return "whoever lined it up can’t send to agents any more";
    default:
      return "herdr said no";
  }
}

/** The Agents row's line under its status, e.g. "2 lined up for when it’s done". */
export function linedUpLine(count: number): string {
  return `${count} lined up for when it’s done`;
}

/** A failed queue.remove: not_found there means the item, not the agent, is gone. */
export function removeErrorText(error: RequestError): string {
  return error.code === "not_found" ? "It’s no longer on the list." : errorText(error);
}

/** herdr's logical key names behind the "…" menu and the unknown sheet. */
export const QUICK_KEYS: { key: string; label: string; name: string }[] = [
  { key: "enter", label: "Enter", name: "Enter" },
  { key: "esc", label: "Esc", name: "Escape" },
  { key: "ctrl+c", label: "Ctrl-C", name: "Control C" },
  { key: "up", label: "↑", name: "Up arrow" },
  { key: "down", label: "↓", name: "Down arrow" },
  ...["1", "2", "3", "4", "5", "6", "7", "8", "9"].map((d) => ({ key: d, label: d, name: d })),
];

/** The answer to "Send a test push", in plain words. */
export function testPushText(result: { ok: true } | { ok: false; error: RequestError }): string {
  if (result.ok) return "Sent. It should show up on this phone in a few seconds.";
  return `Not sent. ${pushErrorText(result.error)}`;
}

/** A failed push request (subscribe, get or test) in plain words. */
export function pushErrorText(error: RequestError): string {
  switch (error.code) {
    case "not_found":
      return "The computer doesn't have this phone's subscription. Open Settings again to send it.";
    case "bad_endpoint":
      return "The computer refused this phone's push address.";
    case "bad_settings":
      return "The computer refused those settings.";
    case "forbidden":
      return "Another sign-in owns this phone's subscription.";
    case "push_failed":
      return "The push service didn't take it. Try again in a minute.";
    case "too_many":
      return "This sign-in already has as many phones as the computer allows. Turn notifications off on one you don't use.";
    case "disconnected":
      return "Not connected.";
    case "lost":
      return "The connection dropped before the computer answered.";
    case "timeout":
      return "No reply from the computer.";
    default:
      return error.message || "Something went wrong.";
  }
}

/** The select's label for a desk window. */
export function deskLabel(minutes: number): string {
  return minutes === 0 ? "Off" : `${minutes} min`;
}

/** What the desk setting does, under it. */
export function deskCaption(minutes: number): string {
  if (minutes === 0) return "Pushes go out even while you're at the agent.";
  return `Held while that agent is focused in herdr and you switched to it in the last ${minutes} minutes. If it still needs you after that, the push goes out.`;
}

/** The agent's last message ends in a question and it's waiting for the next one. */
export function isAsking(status: Status, asks: boolean): boolean {
  return asks && status !== "working" && status !== "blocked";
}

/** The agent view's top card label. */
export function cardLabel(status: Status, asks: boolean): string {
  if (status === "working") return "NOW";
  if (isAsking(status, asks)) return "IT ASKS";
  return status === "done" ? "IT SAID" : "LAST SAID";
}

/** The message field's placeholder; linesUp is true while a working agent's messages are lined up. */
export function composerPlaceholder(status: Status, asks: boolean, linesUp: boolean): string {
  if (linesUp) return "Message · goes when it’s done";
  if (isAsking(status, asks)) return "Reply";
  if (status === "done") return "Give it the next task";
  if (status === "idle") return "Give it a task";
  return "Message";
}

/** The Next list's PR row from the free-text pr token: "#18 ci ✓" reads "PR #18 · ci ✓". */
export function prLabel(pr: string): string {
  const m = /^(#\d+)\s*(?:·\s*)?(.*)$/.exec(pr.trim());
  if (!m) return `PR ${pr.trim()}`;
  return m[2] ? `PR ${m[1]} · ${m[2]}` : `PR ${m[1]}`;
}

/** The /clear confirmation's body. */
export function clearBody(name: string): string {
  return `This sends /clear to ${name}. It wipes its conversation and can’t be undone.`;
}

/** The Next list's /clear row sub-line, or null when the context use isn't known. */
export function contextLine(pct: number | null): string | null {
  return pct === null ? null : `Context used: ${pct}%`;
}
