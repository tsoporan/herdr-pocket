import { expect, test } from "bun:test";
import type { Agent, StartJob, Workspace } from "./protocol";
import {
  JOB_ID_PATTERN,
  NAME_PATTERN,
  agentCount,
  defaultWorkspace,
  failedBeforeTab,
  jobEnding,
  jobSteps,
  kindList,
  nameFromTask,
  nameProblem,
  newJobId,
  startErrorText,
  tabLeftOpen,
  takenNames,
  taskProblem,
  uniqueName,
  workspaceList,
  workspacePath,
} from "./start";

const T = "2026-11-22T10:00:00Z";

function job(extra: Partial<StartJob>): StartJob {
  return { id: "job-fixture-1", by: "fixture@example.test", workspace_id: "w1", kind: "claude", name: "update-v2-api-docs", state: "running", began: T, ...extra };
}

function ws(workspace_id: string, label: string, number: number, extra: Partial<Workspace> = {}): Workspace {
  return { workspace_id, label, number, ...extra };
}

test("nameFromTask: the first words as a name herdr accepts", () => {
  expect(nameFromTask("Update the v2 API docs with the new rate limit")).toBe("update-v2-api-docs");
  expect(nameFromTask("  fix   login_bug!!  ")).toBe("fix-login-bug");
  expect(nameFromTask("Café menu résumé")).toBe("cafe-menu-resume");
  expect(nameFromTask("2fa login")).toBe("agent-2fa-login");
  expect(nameFromTask("日本語のタスク")).toBe("agent");
  expect(nameFromTask("")).toBe("agent");
  expect(nameFromTask("the and of")).toBe("agent");
  const long = nameFromTask("Internationalization accessibility documentation reorganization");
  expect(long.length).toBeLessThanOrEqual(32);
  expect(long).toBe("internationalization");
  expect(nameFromTask("Supercalifragilisticexpialidocious-ly-long-word")).toBe("supercalifragilisticexpialidocio");
  for (const task of ["Update the v2 API docs", "2fa login", "日本語", "x".repeat(100), "a-b c_d", "Zz 99 zz"]) {
    expect(nameFromTask(task)).toMatch(NAME_PATTERN);
  }
});

test("uniqueName adds -2, -3 within 32 characters", () => {
  expect(uniqueName("docs", new Set())).toBe("docs");
  expect(uniqueName("docs", new Set(["docs"]))).toBe("docs-2");
  expect(uniqueName("docs", new Set(["docs", "docs-2"]))).toBe("docs-3");
  const base = "a".repeat(32);
  const name = uniqueName(base, new Set([base]));
  expect(name).toBe(`${"a".repeat(30)}-2`);
  expect(name).toMatch(NAME_PATTERN);
  // Cut at a dash, never leaving one before the suffix.
  expect(uniqueName("update-v2-api-docs-and-the-tests", new Set(["update-v2-api-docs-and-the-tests"]))).toBe("update-v2-api-docs-and-the-2");
});

test("takenNames: live agents' herdr names and jobs still starting", () => {
  // An unnamed agent shows as its kind ("claude"); herdr would accept that name.
  const agents = [{ name: "docs", herdr_name: "docs" }, { name: "infra", herdr_name: "infra" }, { name: "claude" }] as Agent[];
  const starts = [job({ name: "starting", state: "running" }), job({ name: "finished", state: "done" })];
  expect([...takenNames(agents, starts)].sort()).toEqual(["docs", "infra", "starting"]);
});

test("nameProblem and taskProblem", () => {
  const taken = new Set(["docs"]);
  expect(nameProblem("docs-2", taken)).toBeNull();
  expect(nameProblem("docs", taken)).toBe("An agent already has that name.");
  expect(nameProblem("", taken)).toBe("Give it a name.");
  expect(nameProblem("Docs", taken)).not.toBeNull();
  expect(nameProblem("2docs", taken)).not.toBeNull();
  expect(nameProblem("a".repeat(33), taken)).toBe("At most 32 characters.");
  expect(taskProblem("  ")).toBe("Describe the task.");
  expect(taskProblem("Do it")).toBeNull();
  // 4,000 bytes, not characters: 1,334 three-byte characters are 4,002.
  expect(taskProblem("é".repeat(2000))).toBeNull();
  expect(taskProblem("語".repeat(1334))).not.toBeNull();
  expect(taskProblem(` ${"x".repeat(4000)} `)).toBeNull();
});

test("defaultWorkspace: the chosen one, else the last opened agent's, else the first", () => {
  const list = workspaceList({ w2: ws("w2", "infra", 2), w1: ws("w1", "api", 1), w3: ws("w3", "web", 3) });
  expect(list.map((w) => w.workspace_id)).toEqual(["w1", "w2", "w3"]);
  expect(defaultWorkspace(list, "w3", "w2")).toBe("w3");
  expect(defaultWorkspace(list, null, "w2")).toBe("w2");
  // A chosen workspace that closed falls back.
  expect(defaultWorkspace(list, "w9", "w2")).toBe("w2");
  expect(defaultWorkspace(list, null, "w9")).toBe("w1");
  expect(defaultWorkspace([], "w1", "w1")).toBeNull();
});

test("workspace and kind words", () => {
  expect(workspacePath(ws("w1", "api", 1, { path: "~/code/api" }))).toBe("~/code/api");
  expect(workspacePath(ws("w1", "api", 1, { path: "" }))).toBe("Its current folder");
  expect(workspacePath(ws("w1", "api", 1))).toBe("Its current folder");
  expect(agentCount(0)).toBe("0 agents");
  expect(agentCount(1)).toBe("1 agent");
  expect(agentCount(3)).toBe("3 agents");
  expect(agentCount(undefined)).toBe("");
  const kinds = kindList([
    { kind: "codex", label: "Codex" },
    { kind: "claude", label: "Claude Code" },
    { kind: "pi", label: "pi" },
  ]);
  expect(kinds.map((k) => k.kind)).toEqual(["claude", "codex", "pi"]);
});

test("newJobId matches the server's job id rule", () => {
  const a = newJobId();
  expect(a).toMatch(JOB_ID_PATTERN);
  expect(newJobId()).not.toBe(a);
});

const marks = (j: StartJob) => jobSteps(j, "api", "Claude Code").map((s) => `${s.mark}:${s.text}`);

test("jobSteps while running", () => {
  expect(marks(job({}))).toEqual(["now:Opened a tab in api", "todo:Started Claude Code", "todo:Sent your task"]);
  expect(marks(job({ opened_at: T }))).toEqual(["ok:Opened a tab in api", "now:Started Claude Code", "todo:Sent your task"]);
  expect(marks(job({ opened_at: T, started_at: T }))).toEqual(["ok:Opened a tab in api", "ok:Started Claude Code", "now:Sent your task"]);
});

test("jobSteps at each ending", () => {
  const all = { opened_at: T, started_at: T, sent_at: T, ended_at: T };
  expect(marks(job({ ...all, state: "done" }))).toEqual(["ok:Opened a tab in api", "ok:Started Claude Code", "ok:Sent your task"]);
  expect(marks(job({ opened_at: T, ended_at: T, state: "asks_first", error: "agent_not_ready" }))).toEqual([
    "ok:Opened a tab in api",
    "ask:Claude Code is waiting for you",
    "todo:Sends your task once it’s ready",
  ]);
  expect(marks(job({ ended_at: T, state: "failed", failed_step: "opened", error: "workspace_not_found" }))).toEqual([
    "fail:Couldn’t open a tab in api",
    "todo:Started Claude Code",
    "todo:Sent your task",
  ]);
  expect(marks(job({ opened_at: T, ended_at: T, state: "failed", failed_step: "started", error: "timeout" }))).toEqual([
    "ok:Opened a tab in api",
    "fail:Claude Code didn’t start",
    "todo:Sent your task",
  ]);
  expect(marks(job({ opened_at: T, started_at: T, ended_at: T, state: "stalled", failed_step: "sent", error: "agent_prompt_stalled" }))).toEqual([
    "ok:Opened a tab in api",
    "ok:Started Claude Code",
    "ask:Your task may not have gone",
  ]);
  // No failed_step: the first step not done.
  expect(marks(job({ opened_at: T, ended_at: T, state: "unsure", error: "unsure" }))).toEqual([
    "ok:Opened a tab in api",
    "ask:Claude Code didn’t start",
    "todo:Sent your task",
  ]);
  // lost_pane after asking first: the task wasn't lined up.
  expect(marks(job({ opened_at: T, ended_at: T, state: "failed", failed_step: "sent", error: "lost_pane" }))[2]).toBe("fail:Your task wasn’t sent");
});

test("jobEnding words per state and code, never the raw code", () => {
  expect(jobEnding(job({}), "Claude Code")).toBeNull();
  expect(jobEnding(job({ state: "done" }), "Claude Code")).toBeNull();
  expect(jobEnding(job({ state: "asks_first", opened_at: T }), "Claude Code")?.title).toBe("It’s asking something before it starts");
  const notInstalled = jobEnding(job({ state: "failed", opened_at: T, tab_id: "w1:t5", failed_step: "started", error: "timeout" }), "Kiro");
  expect(notInstalled).toEqual({
    tone: "fail",
    title: "Kiro didn’t start",
    body: "It didn’t start in time. Is Kiro installed on the computer? The tab is still open; close it from the desktop.",
  });
  const noTab = jobEnding(job({ state: "failed", failed_step: "opened", error: "workspace_not_found" }), "Claude Code");
  expect(noTab?.body).toBe("That workspace isn’t open on the desktop any more.");
  const odd = jobEnding(job({ state: "failed", opened_at: T, failed_step: "started", error: "some_new_code" }), "Codex");
  expect(odd?.body).toBe("herdr couldn’t start it. The tab is still open; close it from the desktop.");
  expect(odd?.body).not.toContain("some_new_code");
  expect(jobEnding(job({ state: "failed", error: "weird" }), "Codex")?.title).toBe("Couldn’t open a tab");
  expect(jobEnding(job({ state: "stalled", opened_at: T, started_at: T }), "Codex")?.title).toBe("It may not have your task");
  // Unsure before a tab is known: it may not exist, so it isn't stated as open.
  const unsure = jobEnding(job({ state: "unsure", failed_step: "opened" }), "Codex")?.body;
  expect(unsure).toContain("A tab may have opened; check the desktop");
  expect(unsure).not.toContain("still open");
  expect(jobEnding(job({ state: "unsure", opened_at: T, failed_step: "started" }), "Codex")?.body).toContain("The tab is still open");
});

test("tabLeftOpen and failedBeforeTab", () => {
  expect(tabLeftOpen(job({ state: "failed", failed_step: "opened" }))).toBe(false);
  expect(failedBeforeTab(job({ state: "failed", failed_step: "opened" }))).toBe(true);
  expect(tabLeftOpen(job({ state: "failed", opened_at: T }))).toBe(true);
  expect(failedBeforeTab(job({ state: "failed", opened_at: T }))).toBe(false);
  // tab.create may have gone through before the link dropped, but no tab is known.
  expect(tabLeftOpen(job({ state: "unsure", failed_step: "opened" }))).toBe(false);
  expect(failedBeforeTab(job({ state: "unsure", failed_step: "opened" }))).toBe(false);
});

test("startErrorText covers the start acks", () => {
  for (const code of ["name_taken", "invalid_name", "not_found", "unknown_kind", "too_long", "read_only", "forbidden", "invalid_request", "disconnected", "lost", "timeout"] as const) {
    const text = startErrorText({ code, message: "raw" });
    expect(text).not.toBe("raw");
    expect(text).not.toContain("this agent");
  }
  expect(startErrorText({ code: "lost", message: "" })).toContain("won’t start a second one");
});
