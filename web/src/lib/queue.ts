// Pure data logic for work lined up for agents:
// which items belong to an agent and in what order. No DOM, no timers.

import type { QueueItem } from "./protocol";

/** True when the item was lined up for this pane, or was a workspace item sent to it. */
function isFor(item: QueueItem, paneId: string): boolean {
  return ("pane_id" in item.target && item.target.pane_id === paneId) || item.sent_to === paneId;
}

function since(item: QueueItem): number {
  return Date.parse(item.sent_at ?? item.added);
}

/**
 * An agent's list as the Lined up section shows it: finished items first, oldest
 * send first, then the waiting ones in the order they go out (oldest added first).
 */
export function agentItems(queue: QueueItem[], paneId: string): QueueItem[] {
  const mine = queue.filter((it) => isFor(it, paneId));
  const done = mine.filter((it) => it.state !== "waiting").sort((a, b) => since(a) - since(b));
  const waiting = mine.filter((it) => it.state === "waiting").sort((a, b) => Date.parse(a.added) - Date.parse(b.added));
  return [...done, ...waiting];
}

/** How many items are still waiting to go to this pane (the Agents row's count). */
export function waitingCount(queue: QueueItem[], paneId: string): number {
  return queue.filter((it) => it.state === "waiting" && "pane_id" in it.target && it.target.pane_id === paneId).length;
}
