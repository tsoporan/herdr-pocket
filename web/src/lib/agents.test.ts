import { describe, expect, test } from "bun:test";
import { ageLabel, applyPatch, fromSnapshot, nextWaiting, sections, waitingPosition } from "./agents";
import type { Agent, Status } from "./protocol";

const NOW = Date.parse("2026-11-20T12:00:00Z");

function agent(pane_id: string, status: Status, secondsAgo: number, state_change_seq = 0): Agent {
  return {
    pane_id,
    name: pane_id,
    agent: "claude",
    status,
    line: "",
    workspace_id: "w1",
    workspace_label: "api",
    tab_id: "t1",
    changed_at: new Date(NOW - secondsAgo * 1000).toISOString(),
    state_change_seq,
    tokens: {},
    focused: false,
  };
}

describe("sections", () => {
  test("herdr's seq orders agents first seen together (the studio fixture)", () => {
    const agents = fromSnapshot({
      type: "snapshot",
      host: "studio",
      role: "controller",
      expiry: null,
      push: { vapid_public_key: "" },
      workspaces: [],
      agents: [
        agent("w1:p2", "working", 0, 114),
        agent("w1:p3", "idle", 0, 101),
        agent("w2:p2", "working", 0, 113),
        agent("w3:p2", "working", 0, 112),
        agent("w4:p1", "working", 0, 111),
        agent("w4:p2", "idle", 0, 104),
      ],
    });
    expect(sections(agents).map((s) => s.rows.map((r) => r.pane_id))).toEqual([
      ["w1:p2", "w2:p2", "w3:p2", "w4:p1"],
      ["w4:p2", "w1:p3"],
    ]);
  });

  test("groups and orders like the mockup", () => {
    const agents = fromSnapshot({
      type: "snapshot",
      host: "studio",
      role: "controller",
      expiry: null,
      push: { vapid_public_key: "" },
      workspaces: [],
      agents: [
        agent("done-40s", "done", 40),
        agent("ask-6m", "blocked", 360),
        agent("ask-2m", "blocked", 120),
        agent("work-a", "working", 30),
        agent("work-b", "working", 10),
        agent("idle-1h", "idle", 3600),
        agent("unknown-5m", "unknown", 300),
        agent("idle-22m", "idle", 1320),
      ],
    });
    expect(sections(agents).map((s) => [s.title, s.rows.map((r) => r.pane_id)])).toEqual([
      ["Needs you", ["ask-2m", "ask-6m", "done-40s"]],
      ["Working", ["work-b", "work-a"]],
      ["Idle", ["unknown-5m", "idle-22m", "idle-1h"]],
    ]);
  });

  test("hides empty sections and breaks ties by pane_id", () => {
    const agents = fromSnapshot({
      type: "snapshot",
      host: "studio",
      role: "controller",
      expiry: null,
      push: { vapid_public_key: "" },
      workspaces: [],
      agents: [agent("b", "idle", 5), agent("a", "idle", 5)],
    });
    expect(sections(agents).map((s) => [s.key, s.rows.map((r) => r.pane_id)])).toEqual([["idle", ["a", "b"]]]);
  });
});

test("applyPatch adds, replaces and removes with null", () => {
  const start = fromSnapshot({
    type: "snapshot",
    host: "studio",
    role: "controller",
    expiry: null,
    push: { vapid_public_key: "" },
    workspaces: [],
    agents: [agent("a", "working", 0), agent("b", "idle", 0)],
  });
  const next = applyPatch(start, {
    type: "patch",
    agents: { a: agent("a", "blocked", 0), b: null, c: agent("c", "done", 0) },
  });
  expect(Object.keys(next).sort()).toEqual(["a", "c"]);
  expect(next.a?.status).toBe("blocked");
  expect(start.b).toBeDefined();
  expect(applyPatch(next, { type: "patch" })).toEqual(next);
});

test("ageLabel", () => {
  const at = (status: Status, s: number) => ageLabel(agent("x", status, s), NOW);
  expect(at("working", 900)).toBe("now");
  expect(at("done", 40)).toBe("40s");
  expect(at("idle", 59)).toBe("59s");
  expect(at("idle", 60)).toBe("1m");
  expect(at("idle", 3599)).toBe("59m");
  expect(at("idle", 3600)).toBe("1h");
  expect(at("idle", 86399)).toBe("23h");
  expect(at("idle", 86400 * 3)).toBe("3d");
  expect(at("blocked", -30)).toBe("now");
  expect(at("done", 9)).toBe("now");
  expect(at("done", 10)).toBe("10s");
});

describe("the waiting queue", () => {
  const agents = fromSnapshot({
    type: "snapshot",
    host: "studio",
    role: "controller",
    expiry: null,
    push: { vapid_public_key: "" },
    workspaces: [],
    agents: [agent("ask-2m", "blocked", 120), agent("ask-6m", "blocked", 360), agent("work", "working", 0)],
  });

  test("waitingPosition counts in list order", () => {
    expect(waitingPosition(agents, "ask-2m")).toEqual({ index: 1, total: 2 });
    expect(waitingPosition(agents, "ask-6m")).toEqual({ index: 2, total: 2 });
    expect(waitingPosition(agents, "work")).toBeNull();
  });

  test("nextWaiting goes on to finished agents once no blocked one is left, as Needs you lists them", () => {
    const withDone = applyPatch(agents, { type: "patch", agents: { fin: agent("fin", "done", 30) } });
    expect(nextWaiting(withDone, "ask-2m")).toBe("ask-6m");
    expect(nextWaiting(withDone, "ask-6m")).toBe("ask-2m");
    const answered = applyPatch(withDone, {
      type: "patch",
      agents: { "ask-2m": agent("ask-2m", "working", 0), "ask-6m": agent("ask-6m", "working", 0) },
    });
    expect(nextWaiting(answered, "ask-6m")).toBe("fin");
    expect(nextWaiting(answered, "fin")).toBeNull();
  });

  test("nextWaiting skips the current agent, then runs out", () => {
    expect(nextWaiting(agents, "ask-2m")).toBe("ask-6m");
    expect(nextWaiting(agents, "work")).toBe("ask-2m");
    const answered = applyPatch(agents, { type: "patch", agents: { "ask-2m": agent("ask-2m", "working", 0) } });
    expect(nextWaiting(answered, "ask-2m")).toBe("ask-6m");
    expect(nextWaiting(answered, "ask-6m")).toBeNull();
  });
});
