import { expect, test } from "bun:test";
import type { QueueItem, QueueState, QuestionOption } from "./protocol";
import {
  agoLabel,
  linkWords,
  cardLabel,
  followLabel,
  clearBody,
  composerPlaceholder,
  contextLine,
  deskCaption,
  deskLabel,
  durationLabel,
  errorText,
  expiryBody,
  expiryTitle,
  lineUpLabel,
  linedUpLine,
  prLabel,
  removeErrorText,
  sentLabel,
  sentSub,
  statusLabel,
  testPushText,
} from "./words";

const MIN = 60_000;

test("durationLabel", () => {
  expect(durationLabel(-5000)).toBe("under a minute");
  expect(durationLabel(59_999)).toBe("under a minute");
  expect(durationLabel(MIN)).toBe("1 min");
  expect(durationLabel(14 * MIN + 30_000)).toBe("14 min");
  expect(durationLabel(59 * MIN)).toBe("59 min");
  expect(durationLabel(60 * MIN)).toBe("1 h");
  expect(durationLabel(125 * MIN)).toBe("2 h 5 min");
  expect(durationLabel(24 * 60 * MIN)).toBe("1 d");
  expect(durationLabel(3 * 24 * 60 * MIN + 5 * MIN)).toBe("3 d");
});

test("statusLabel", () => {
  const now = Date.parse("2026-11-20T12:14:00Z");
  const since = "2026-11-20T12:00:00Z";
  expect(statusLabel("working", since, now)).toBe("Working for 14 min");
  expect(statusLabel("blocked", "2026-11-20T12:12:00Z", now)).toBe("Waiting for you · 2 min");
  expect(statusLabel("done", since, now)).toBe("Finished 14 min ago");
  expect(statusLabel("done", "2026-11-20T12:13:30Z", now)).toBe("Finished just now");
  expect(statusLabel("idle", since, now)).toBe("Idle for 14 min");
  expect(statusLabel("unknown", since, now)).toBe("Can't tell what it's doing");
});

function option(label: string, needs_note = false): QuestionOption {
  return { key: "1", label, sub: "", primary: false, needs_note, own: false };
}

test("sentLabel uses the mockup's words, with a fallback", () => {
  expect(sentLabel(option("Allow once"), undefined)).toBe("Allowed once");
  expect(sentLabel(option("Always allow"), undefined)).toBe("Always allowed");
  expect(sentLabel(option("Don’t allow", true), "use a temp dir")).toBe("Declined, with your note");
  expect(sentLabel(option("Don’t allow", true), undefined)).toBe("Declined");
  expect(sentLabel(option("Staging first"), undefined)).toBe("Chose “Staging first”");
  expect(sentLabel(option("Something else", true), "dev, then staging")).toBe("Sent your note");
  expect(sentLabel({ ...option("Your own answer", true), own: true }, "After the backup")).toBe("Sent your answer");
  expect(sentLabel(option("Send answers"), undefined)).toBe("Sent your answers");
  expect(sentSub(option("Cancel the question"))).toBe("It's waiting for a message now.");
  expect(sentSub(option("Send answers"))).toBe("The agent is working again.");
});

test("errorText", () => {
  expect(errorText({ code: "question_changed", message: "x" })).toBe(
    "The question changed on the desktop. Here's the new one.",
  );
  expect(errorText({ code: "read_only", message: "" })).toBe("This server is read-only, so nothing was sent.");
  expect(errorText({ code: "forbidden", message: "" })).toBe("You can view this agent but not control it.");
  expect(errorText({ code: "disconnected", message: "" })).toBe("Not sent: no connection right now.");
  expect(errorText({ code: "lost", message: "" })).toContain("may have gone");
  // A code newer than this app falls back to the server's message.
  const future = { code: "rate_limited", message: "Slow down." } as unknown as Parameters<typeof errorText>[0];
  expect(errorText(future)).toBe("Slow down.");
  expect(errorText({ ...future, message: "" })).toBe("Something went wrong.");
});

test("expiryTitle counts whole days down to ended", () => {
  expect(expiryTitle(9)).toBe("Tailscale sign-in ends in 9 days");
  expect(expiryTitle(2)).toBe("Tailscale sign-in ends in 2 days");
  expect(expiryTitle(1)).toBe("Tailscale sign-in ends tomorrow");
  expect(expiryTitle(0)).toBe("Tailscale sign-in has ended");
  expect(expiryTitle(-3)).toBe("Tailscale sign-in has ended");
});

test("expiryBody names the computer", () => {
  expect(expiryBody(9, "studio")).toBe(
    "After that this app can't reach studio. Turn off key expiry for herdr-pocket, or sign it in again.",
  );
  expect(expiryBody(1, "studio")).toBe(
    "After that this app can't reach studio. Turn off key expiry for herdr-pocket, or sign it in again.",
  );
  expect(expiryBody(0, "studio")).toBe("This app can't reach studio until it's signed in again.");
  expect(expiryBody(-1, "studio")).toBe("This app can't reach studio until it's signed in again.");
});

test("testPushText", () => {
  expect(testPushText({ ok: true })).toBe("Sent. It should show up on this phone in a few seconds.");
  expect(testPushText({ ok: false, error: { code: "timeout", message: "no ack" } })).toBe("Not sent. No reply from the computer.");
  expect(testPushText({ ok: false, error: { code: "bad_endpoint", message: "x" } })).toBe(
    "Not sent. The computer refused this phone's push address.",
  );
  expect(testPushText({ ok: false, error: { code: "push_failed", message: "the push failed" } })).toBe(
    "Not sent. The push service didn't take it. Try again in a minute.",
  );
  expect(testPushText({ ok: false, error: { code: "too_many", message: "x" } })).toBe(
    "Not sent. This sign-in already has as many phones as the computer allows. Turn notifications off on one you don't use.",
  );
  expect(testPushText({ ok: false, error: { code: "herdr_error", message: "something else" } })).toBe(
    "Not sent. something else",
  );
});

test("desk words", () => {
  expect(deskLabel(0)).toBe("Off");
  expect(deskLabel(5)).toBe("5 min");
  expect(deskCaption(0)).toBe("Pushes go out even while you're at the agent.");
  expect(deskCaption(10)).toContain("in the last 10 minutes");
});

function item(state: QueueState, extra: Partial<QueueItem> = {}): QueueItem {
  return {
    id: "q1",
    target: { pane_id: "w1:p2" },
    text: "Then update the v2 docs with the new limit",
    by: "fixture@example.test",
    added: "2026-11-20T11:00:00Z",
    state,
    ...extra,
  };
}

test("lineUpLabel", () => {
  const now = Date.parse("2026-11-20T12:14:00Z");
  const sent = { sent_at: "2026-11-20T11:43:00Z" };
  expect(lineUpLabel(item("waiting"), true, now)).toBe("Next, when it’s done");
  expect(lineUpLabel(item("waiting"), false, now)).toBe("After that");
  expect(lineUpLabel(item("sent", sent), false, now)).toBe("Sent 31 min ago");
  expect(lineUpLabel(item("sent", { sent_at: "2026-11-20T12:13:30Z" }), false, now)).toBe("Sent just now");
  expect(lineUpLabel(item("picked_up", sent), false, now)).toBe("Sent 31 min ago · it picked it up");
  expect(lineUpLabel(item("stalled", sent), false, now)).toBe("Sent, but it didn’t start");
  expect(lineUpLabel(item("failed", { error: "agent_blocked" }), false, now)).toBe("Couldn’t send: it was waiting for an answer");
  expect(lineUpLabel(item("failed", { error: "pane_not_found" }), false, now)).toBe("Couldn’t send: the agent wasn’t there");
  // Never the raw text: an older list may hold a Go error with a socket path.
  expect(lineUpLabel(item("failed", { error: "dial unix /run/user/1000/herdr.sock: connect" }), false, now)).toBe(
    "Couldn’t send: herdr said no",
  );
  expect(lineUpLabel(item("failed"), false, now)).toBe("Couldn’t send: herdr said no");
  expect(lineUpLabel(item("dropped"), false, now)).toBe("Not sent: the agent closed");
});

test("linedUpLine", () => {
  expect(linedUpLine(1)).toBe("1 lined up for when it’s done");
  expect(linedUpLine(2)).toBe("2 lined up for when it’s done");
});

test("queue errors", () => {
  expect(errorText({ code: "queue_full", message: "" })).toBe("Its list is full. Remove one before lining up more.");
  expect(errorText({ code: "queue_failed", message: "" })).toBe("The computer couldn’t save the list, so nothing changed. Try again.");
  expect(errorText({ code: "not_waiting", message: "" })).toBe("That one has already gone, so it can’t be removed.");
  expect(removeErrorText({ code: "not_found", message: "" })).toBe("It’s no longer on the list.");
  expect(removeErrorText({ code: "forbidden", message: "" })).toBe(errorText({ code: "forbidden", message: "" }));
});

test("cardLabel: NOW while working, IT ASKS only on a question, else what it said", () => {
  expect(cardLabel("working", false)).toBe("NOW");
  expect(cardLabel("working", true)).toBe("NOW");
  expect(cardLabel("done", true)).toBe("IT ASKS");
  expect(cardLabel("idle", true)).toBe("IT ASKS");
  expect(cardLabel("unknown", true)).toBe("IT ASKS");
  expect(cardLabel("done", false)).toBe("IT SAID");
  expect(cardLabel("idle", false)).toBe("LAST SAID");
  expect(cardLabel("unknown", false)).toBe("LAST SAID");
});

test("composerPlaceholder by state", () => {
  expect(composerPlaceholder("done", true, false)).toBe("Reply");
  expect(composerPlaceholder("idle", true, false)).toBe("Reply");
  expect(composerPlaceholder("done", false, false)).toBe("Give it the next task");
  expect(composerPlaceholder("idle", false, false)).toBe("Give it a task");
  expect(composerPlaceholder("working", true, true)).toBe("Message · goes when it’s done");
  expect(composerPlaceholder("working", true, false)).toBe("Message");
  expect(composerPlaceholder("unknown", false, false)).toBe("Message");
});

test("prLabel", () => {
  expect(prLabel("#18 ci ✓")).toBe("PR #18 · ci ✓");
  expect(prLabel("#51 · checks running")).toBe("PR #51 · checks running");
  expect(prLabel("#78")).toBe("PR #78");
  expect(prLabel("draft")).toBe("PR draft");
});

test("clearBody and contextLine", () => {
  expect(clearBody("Fix flaky checkout test")).toBe(
    "This sends /clear to Fix flaky checkout test. It wipes its conversation and can’t be undone.",
  );
  expect(contextLine(63)).toBe("Context used: 63%");
  expect(contextLine(null)).toBeNull();
});

test("followLabel: what happens to a message lined up from the field", () => {
  const base = { id: "q1", target: { pane_id: "w1:p1" }, text: "x", by: "me", added: "2026-11-22T12:00:00Z" } as const;
  const now = Date.parse("2026-11-22T12:01:00Z");
  expect(followLabel(undefined, 0, now)).toBe("Lining up…");
  expect(followLabel({ ...base, state: "waiting" }, 0, now)).toBe("Lined up · goes when it finishes");
  expect(followLabel({ ...base, state: "waiting" }, 2, now)).toBe("Lined up · goes when it finishes, after 2 more");
  expect(followLabel({ ...base, state: "sent", sent_at: "2026-11-22T12:00:30Z" }, 0, now)).toBe("Sent · waiting for it to start");
  expect(followLabel({ ...base, state: "picked_up" }, 0, now)).toBe("✓ Sent · it picked it up");
  expect(followLabel({ ...base, state: "failed", error: "agent_blocked" }, 0, now)).toBe(
    "Couldn’t send: it was waiting for an answer",
  );
});

test("agoLabel counts seconds under a minute", () => {
  const now = Date.parse("2026-11-22T12:00:00Z");
  expect(agoLabel(null, now)).toBe("nothing yet");
  expect(agoLabel(now - 2000, now)).toBe("just now");
  expect(agoLabel(now - 42000, now)).toBe("42 s ago");
  expect(agoLabel(now - 3 * 60000, now)).toBe("3 min ago");
});

test("linkWords says what each state means for what's on screen", () => {
  const now = Date.parse("2026-11-22T12:00:00Z");
  const last = now - 42000;
  expect(linkWords("live", null, last, now)).toEqual({ title: "Connected", detail: null });
  expect(linkWords("slow", null, last, now)).toEqual({ title: "Slow connection", detail: "Checking it… Last update 42 s ago." });
  expect(linkWords("down", now + 3000, last, now).title).toBe("Can’t reach the computer · trying again in 3 s");
  expect(linkWords("down", now - 10, last, now).title).toBe("Can’t reach the computer · trying again");
  expect(linkWords("offline", null, last, now)).toEqual({
    title: "You’re offline",
    detail: "Last update 42 s ago. It reconnects when the network is back.",
  });
  expect(linkWords("connecting", null, null, now)).toEqual({ title: "Connecting…", detail: null });
});
