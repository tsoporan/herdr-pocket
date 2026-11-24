// Pure data logic for the Agents list: applying server messages, grouping
// into sections, ordering, age labels and the waiting queue. No DOM, no timers.

import type { Agent, Patch, Snapshot, Status } from "./protocol";

export type Agents = Record<string, Agent>;

export function fromSnapshot(snapshot: Snapshot): Agents {
  return Object.fromEntries(snapshot.agents.map((a) => [a.pane_id, a]));
}

export function applyPatch(agents: Agents, patch: Patch): Agents {
  const next = { ...agents };
  for (const [id, agent] of Object.entries(patch.agents ?? {})) {
    if (agent === null) delete next[id];
    else next[id] = agent;
  }
  return next;
}

/** Visual kind of a row, matching the mockup's T table. Unknown looks idle. */
export type Kind = "ask" | "done" | "working" | "idle";

export function kindOf(status: Status): Kind {
  switch (status) {
    case "blocked":
      return "ask";
    case "done":
      return "done";
    case "working":
      return "working";
    default:
      return "idle";
  }
}

export type SectionKey = "needs" | "working" | "idle";

export interface Section {
  key: SectionKey;
  title: string;
  rows: Agent[];
}

const SECTIONS: { key: SectionKey; title: string }[] = [
  { key: "needs", title: "Needs you" },
  { key: "working", title: "Working" },
  { key: "idle", title: "Idle" },
];

function sectionOf(status: Status): SectionKey {
  if (status === "blocked" || status === "done") return "needs";
  if (status === "working") return "working";
  return "idle";
}

// Within Needs you, blocked agents come before done ones.
const RANK: Record<Status, number> = { blocked: 0, done: 1, working: 0, idle: 0, unknown: 0 };

function compare(a: Agent, b: Agent): number {
  return (
    RANK[a.status] - RANK[b.status] ||
    Date.parse(b.changed_at) - Date.parse(a.changed_at) ||
    b.state_change_seq - a.state_change_seq ||
    a.pane_id.localeCompare(b.pane_id)
  );
}

/** The non-empty sections in display order, rows sorted newest change first. */
export function sections(agents: Agents): Section[] {
  const all = Object.values(agents);
  return SECTIONS.map(({ key, title }) => ({
    key,
    title,
    rows: all.filter((a) => sectionOf(a.status) === key).sort(compare),
  })).filter((s) => s.rows.length > 0);
}

/** "now" for working agents or under 10s, otherwise 40s, 2m, 1h, 3d since changed_at. */
export function ageLabel(agent: Agent, now: number): string {
  if (agent.status === "working") return "now";
  // Clamp: the phone's clock may run behind the server's.
  const s = Math.max(0, Math.floor((now - Date.parse(agent.changed_at)) / 1000));
  if (s < 10) return "now";
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

/** Blocked agents in the order the list shows them. */
function waiting(agents: Agents): Agent[] {
  return Object.values(agents)
    .filter((a) => a.status === "blocked")
    .sort(compare);
}

/** "1 of 3 waiting": this agent's place among the blocked ones, or null if it isn't blocked. */
export function waitingPosition(agents: Agents, paneId: string): { index: number; total: number } | null {
  const queue = waiting(agents);
  const i = queue.findIndex((a) => a.pane_id === paneId);
  return i < 0 ? null : { index: i + 1, total: queue.length };
}

/**
 * The next agent in Needs you other than this one, in list order (blocked
 * ones first, then finished ones, as the list shows them), or null when none
 * is left.
 */
export function nextWaiting(agents: Agents, paneId: string): string | null {
  return (
    Object.values(agents)
      .filter((a) => sectionOf(a.status) === "needs" && a.pane_id !== paneId)
      .sort(compare)[0]?.pane_id ?? null
  );
}
