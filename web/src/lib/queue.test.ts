import { expect, test } from "bun:test";
import type { QueueItem, QueueState, QueueTarget } from "./protocol";
import { agentItems, waitingCount } from "./queue";

function item(id: string, target: QueueTarget, state: QueueState, added: string, extra: Partial<QueueItem> = {}): QueueItem {
  return { id, target, text: `text ${id}`, by: "fixture@example.test", added, state, ...extra };
}

const P = { pane_id: "w1:p2" };
const OTHER = { pane_id: "w1:p3" };
const WS = { workspace_id: "w1" };

const queue: QueueItem[] = [
  item("b", P, "waiting", "2026-11-20T11:20:00Z"),
  item("a", P, "picked_up", "2026-11-20T11:00:00Z", { sent_at: "2026-11-20T11:43:00Z" }),
  item("c", P, "waiting", "2026-11-20T11:10:00Z"),
  item("x", OTHER, "waiting", "2026-11-20T11:05:00Z"),
  // A workspace item sent to this pane belongs to its list; one still waiting does not.
  item("w", WS, "sent", "2026-11-20T10:00:00Z", { sent_at: "2026-11-20T11:30:00Z", sent_to: "w1:p2" }),
  item("v", WS, "waiting", "2026-11-20T10:30:00Z"),
];

test("agentItems: finished first by send time, then waiting by added", () => {
  expect(agentItems(queue, "w1:p2").map((it) => it.id)).toEqual(["w", "a", "c", "b"]);
  expect(agentItems(queue, "w1:p3").map((it) => it.id)).toEqual(["x"]);
  expect(agentItems(queue, "w9:p9")).toEqual([]);
});

test("waitingCount counts only this pane's waiting items", () => {
  expect(waitingCount(queue, "w1:p2")).toBe(2);
  expect(waitingCount(queue, "w1:p3")).toBe(1);
  expect(waitingCount([], "w1:p2")).toBe(0);
});
