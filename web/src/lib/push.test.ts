import { describe, expect, test } from "bun:test";
import type { Agents } from "./agents";
import type { Agent, Status } from "./protocol";
import { keyBytes, lostSubscription, pushOn, sameKey, staleNotifications, tagPane } from "./push";

test("pushOn needs permission, a subscription and the computer's ok", () => {
  expect(pushOn("granted", true, true)).toBe(true);
  expect(pushOn("granted", true, false)).toBe(false);
  expect(pushOn("granted", false, true)).toBe(false);
  expect(pushOn("default", true, true)).toBe(false);
  expect(pushOn("denied", true, true)).toBe(false);
});

test("lostSubscription", () => {
  expect(lostSubscription("not_found")).toBe(true);
  expect(lostSubscription("forbidden")).toBe(true);
  expect(lostSubscription("timeout")).toBe(false);
  expect(lostSubscription("bad_settings")).toBe(false);
  expect(lostSubscription("push_failed")).toBe(false);
});

function agent(pane_id: string, status: Status): Agent {
  return {
    pane_id,
    name: pane_id,
    agent: "claude",
    status,
    line: "",
    workspace_id: "w1",
    workspace_label: "api",
    tab_id: "t1",
    changed_at: "2026-11-22T12:00:00Z",
    state_change_seq: 1,
    tokens: {},
    focused: false,
  };
}

describe("keyBytes", () => {
  test("decodes base64url without padding", () => {
    expect([...keyBytes("AP8-_w")]).toEqual([0x00, 0xff, 0x3e, 0xff]);
    expect([...keyBytes("BA")]).toEqual([0x04]);
  });

  test("sameKey compares the subscription's key bytes", () => {
    expect(sameKey(new Uint8Array([0, 255, 62, 255]).buffer, "AP8-_w")).toBe(true);
    expect(sameKey(new Uint8Array([0, 255, 62]).buffer, "AP8-_w")).toBe(false);
    expect(sameKey(new Uint8Array([1, 255, 62, 255]).buffer, "AP8-_w")).toBe(false);
    expect(sameKey(null, "AP8-_w")).toBe(false);
  });
});

test("tagPane", () => {
  expect(tagPane("agent-w1:p2")).toBe("w1:p2");
  expect(tagPane("agent-")).toBeNull();
  expect(tagPane("expiry")).toBeNull();
});

test("staleNotifications closes alerts whose agent moved on", () => {
  const agents: Agents = {
    "w1:p1": agent("w1:p1", "blocked"),
    "w1:p2": agent("w1:p2", "working"),
    "w1:p3": agent("w1:p3", "done"),
    "w1:p4": agent("w1:p4", "idle"),
  };
  const n = (tag: string, kind: string) => ({ tag, data: { url: "/", kind } });
  const shown = [
    n("agent-w1:p1", "blocked"), // still blocked: kept
    n("agent-w1:p2", "blocked"), // answered: closed
    n("agent-w9:p9", "blocked"), // gone: closed
    n("agent-w1:p3", "done"), // still done: kept
    n("agent-w1:p4", "done"), // idle since: closed
    n("expiry", "expiry"), // not an agent's: kept
    n("agent-w1:p4", "test"), // not an alert: kept
    { tag: "agent-w1:p2", data: null }, // no data: kept
  ];
  expect(staleNotifications(shown, agents).map((x) => x.tag)).toEqual(["agent-w1:p2", "agent-w9:p9", "agent-w1:p4"]);
});
