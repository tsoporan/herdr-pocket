// Pure logic for New task:
// the agent's name from the task, the default workspace, and the words for a
// start job's steps and ending. No DOM, no timers.

import type { Agent, StartJob, StartKind, StartStep, Workspace } from "./protocol";
import type { RequestError } from "./socket";

/** herdr's rule for an agent name. */
export const NAME_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
export const NAME_MAX = 32;
/** The server's bound on the task, in UTF-8 bytes after trimming. */
export const TASK_MAX_BYTES = 4000;
/** The server's rule for a job id. */
export const JOB_ID_PATTERN = /^[A-Za-z0-9_-]{8,64}$/;

/**
 * How long the server may still hold a job: it forgets one an hour after it
 * ends (sooner past 20 finished, or on a restart). An unacked id older than
 * this isn't resent.
 */
export const JOB_ID_TTL_MS = 60 * 60 * 1000;

/** A fresh job id; a retry resends the same one, so the server never opens a second tab. */
export function newJobId(): string {
  return crypto.randomUUID();
}

// Words that say little in a name; "fix-the-login-bug" reads as "fix-login-bug".
const FILLER = new Set(["a", "an", "the", "and", "or", "to", "of", "for", "with", "in", "on", "at", "by", "it", "its", "this", "that", "please"]);
const NAME_WORDS = 4;

/** Cuts a slug to max characters, at a dash when there is one, with no dash left at the end. */
function cut(slug: string, max: number): string {
  if (slug.length <= max) return slug;
  const head = slug.slice(0, max);
  const dash = head.lastIndexOf("-");
  return (dash > 0 ? head.slice(0, dash) : head).replace(/-+$/, "");
}

/** The task's first few words as a name herdr accepts: "Update the v2 API docs" is "update-v2-api-docs". */
export function nameFromTask(task: string): string {
  const words = task
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "") // é is e
    .split(/[^a-z0-9]+/)
    .filter((w) => w && !FILLER.has(w))
    .slice(0, NAME_WORDS);
  if (!words.length) return "agent";
  let slug = words.join("-");
  // A name must start with a letter: "2fa login" is "agent-2fa-login".
  if (!/^[a-z]/.test(slug)) slug = `agent-${slug}`;
  return cut(slug, NAME_MAX);
}

/** base, or base-2, base-3, … : the first that isn't taken, still within 32 characters. */
export function uniqueName(base: string, taken: ReadonlySet<string>): string {
  if (!taken.has(base)) return base;
  for (let n = 2; ; n++) {
    const suffix = `-${n}`;
    const name = `${cut(base, NAME_MAX - suffix.length)}${suffix}`;
    if (!taken.has(name)) return name;
  }
}

/**
 * The names a new agent can't have: the live agents' own herdr names (not a
 * title shown in their place), and those of jobs still starting.
 */
export function takenNames(agents: Iterable<Agent>, starts: readonly StartJob[]): Set<string> {
  const names = new Set<string>();
  for (const a of agents) if (a.herdr_name) names.add(a.herdr_name);
  for (const j of starts) if (j.state === "running") names.add(j.name);
  return names;
}

/** What's wrong with a typed name, or null. The server checks again; this is the live hint. */
export function nameProblem(name: string, taken: ReadonlySet<string>): string | null {
  if (!name) return "Give it a name.";
  if (name.length > NAME_MAX) return "At most 32 characters.";
  if (!NAME_PATTERN.test(name)) return "Lowercase letters, digits, - and _, starting with a letter.";
  if (taken.has(name)) return "An agent already has that name.";
  return null;
}

/** What's wrong with the task, or null. */
export function taskProblem(task: string): string | null {
  const text = task.trim();
  if (!text) return "Describe the task.";
  if (new TextEncoder().encode(text).length > TASK_MAX_BYTES) return "That’s too long: 4,000 characters at most.";
  return null;
}

/** The workspaces in herdr's order. */
export function workspaceList(workspaces: Record<string, Workspace>): Workspace[] {
  return Object.values(workspaces).sort((a, b) => a.number - b.number || a.label.localeCompare(b.label));
}

/** The chosen workspace if it's still open, else that of the agent last opened, else the first. */
export function defaultWorkspace(list: readonly Workspace[], chosen: string | null, lastOpened: string | null): string | null {
  for (const id of [chosen, lastOpened]) if (id && list.some((w) => w.workspace_id === id)) return id;
  return list[0]?.workspace_id ?? null;
}

/** The kinds in the picker's order: Claude Code first, then as the server lists them. */
export function kindList(kinds: readonly StartKind[]): StartKind[] {
  return [...kinds].sort((a, b) => Number(b.kind === "claude") - Number(a.kind === "claude"));
}

/** "Its current folder" when the workspace has no agents to say where they work. */
export function workspacePath(ws: Workspace): string {
  return ws.path || "Its current folder";
}

export function agentCount(n: number | undefined): string {
  if (n === undefined) return "";
  return n === 1 ? "1 agent" : `${n} agents`;
}

export function kindLabel(kind: string, kinds: readonly StartKind[]): string {
  return kinds.find((k) => k.kind === kind)?.label ?? kind;
}

export function isFinished(job: StartJob): boolean {
  return job.state !== "running";
}

/** A tab was opened, so the person has one to close on the desktop. */
export function tabLeftOpen(job: StartJob): boolean {
  return !!(job.opened_at || job.tab_id);
}

/** Ended before a tab opened: nothing to close, so the task goes straight back in the field. */
export function failedBeforeTab(job: StartJob): boolean {
  return job.state === "failed" && !job.opened_at && !job.tab_id;
}

export type StepMark = "ok" | "now" | "ask" | "fail" | "todo";

export interface StepView {
  step: StartStep;
  mark: StepMark;
  text: string;
}

const STEPS: readonly StartStep[] = ["opened", "started", "sent"];

function stepDone(job: StartJob, step: StartStep): boolean {
  return !!(step === "opened" ? job.opened_at : step === "started" ? job.started_at : job.sent_at);
}

/** The step it ended at: the server's, else the first not done. */
function endStep(job: StartJob): StartStep {
  return job.failed_step ?? STEPS.find((s) => !stepDone(job, s)) ?? "sent";
}

/** The three steps of the mockup's progress list. */
export function jobSteps(job: StartJob, workspace: string, kind: string): StepView[] {
  const done: Record<StartStep, string> = {
    opened: `Opened a tab in ${workspace}`,
    started: `Started ${kind}`,
    sent: "Sent your task",
  };
  const failed: Record<StartStep, string> = {
    opened: `Couldn’t open a tab in ${workspace}`,
    started: `${kind} didn’t start`,
    sent: job.state === "failed" ? "Your task wasn’t sent" : "Your task may not have gone",
  };
  if (job.state === "done") return STEPS.map((step) => ({ step, mark: "ok", text: done[step] }));
  if (job.state === "asks_first") {
    return [
      { step: "opened", mark: "ok", text: done.opened },
      { step: "started", mark: "ask", text: `${kind} is waiting for you` },
      { step: "sent", mark: "todo", text: "Sends your task once it’s ready" },
    ];
  }
  const at = job.state === "running" ? STEPS.find((s) => !stepDone(job, s)) ?? "sent" : endStep(job);
  const atMark: StepMark = job.state === "running" ? "now" : job.state === "failed" ? "fail" : "ask";
  const i = STEPS.indexOf(at);
  return STEPS.map((step, j) => {
    if (j < i) return { step, mark: "ok", text: done[step] };
    if (j > i) return { step, mark: "todo", text: done[step] };
    return { step, mark: atMark, text: atMark === "now" ? done[step] : failed[step] };
  });
}

export interface Ending {
  tone: "ask" | "fail";
  title: string;
  body: string;
}

const LEFT_OPEN = "The tab is still open; close it from the desktop.";

/** Why it failed, in plain words for the step and herdr's code; never herdr's text. */
function failedBody(job: StartJob, kind: string): string {
  const step = endStep(job);
  switch (job.error) {
    case "workspace_not_found":
      return "That workspace isn’t open on the desktop any more.";
    case "timeout":
      return step === "started" ? `It didn’t start in time. Is ${kind} installed on the computer?` : "herdr didn’t answer in time.";
    case "agent_name_taken":
      return "Another agent took that name meanwhile. Pick another and try again.";
    case "invalid_agent_name":
      return "herdr didn’t accept that name. Pick another and try again.";
    case "agent_pane_busy":
      return "The new tab wasn’t free to start it in.";
    case "agent_blocked":
      return "It’s asking something, so your task wasn’t sent. Open it to answer.";
    case "lost_pane":
      return "The new agent never showed up here, so your task wasn’t lined up.";
    default:
      return step === "opened" ? "herdr couldn’t open a tab." : step === "started" ? "herdr couldn’t start it." : "herdr couldn’t send your task.";
  }
}

/** The card under the steps once it has ended, or null while running or when done. */
export function jobEnding(job: StartJob, kind: string): Ending | null {
  const tail = tabLeftOpen(job) ? ` ${LEFT_OPEN}` : "";
  switch (job.state) {
    case "running":
    case "done":
      return null;
    case "asks_first":
      return {
        tone: "ask",
        title: "It’s asking something before it starts",
        body: "Often “Do you trust this folder?”. Your task is kept and sent once it’s ready.",
      };
    case "stalled":
      return {
        tone: "fail",
        title: "It may not have your task",
        body: `It didn’t start working after the task went, so it wasn’t sent again. Open it to check.${tail}`,
      };
    case "unsure":
      return {
        tone: "fail",
        title: "Not sure it went through",
        // Unsure at opened: nobody knows whether tab.create went through.
        body: tail
          ? `The link to herdr dropped partway, so it may have. Check the desktop before trying again.${tail}`
          : "The link to herdr dropped partway, so it may have. A tab may have opened; check the desktop before trying again.",
      };
    case "failed": {
      const step = endStep(job);
      const title = step === "opened" ? "Couldn’t open a tab" : step === "started" ? `${kind} didn’t start` : "Your task wasn’t sent";
      return { tone: "fail", title, body: `${failedBody(job, kind)}${tail}` };
    }
  }
}

/** The form's error line for a refused or lost start request. The name codes show on the Name row. */
export function startErrorText(error: RequestError): string {
  switch (error.code) {
    case "name_taken":
      return "An agent already has that name.";
    case "invalid_name":
      return "herdr won’t accept that name: lowercase letters, digits, - and _, starting with a letter.";
    case "not_found":
      return "That workspace isn’t open on the desktop any more.";
    case "unknown_kind":
      return "This computer doesn’t start that kind of agent.";
    case "too_long":
      return "That task is too long: 4,000 characters at most.";
    case "read_only":
      return "This server is read-only, so nothing started.";
    case "forbidden":
      return "You can view agents but not start them.";
    case "invalid_request":
      return "herdr-pocket couldn’t send that.";
    case "disconnected":
      return "Not sent: no connection right now.";
    case "lost":
    case "timeout":
      return "No answer from the computer yet. Trying again won’t start a second one.";
    default:
      return "Something went wrong, so nothing started.";
  }
}
