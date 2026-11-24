<script lang="ts">
  // New task:
  // a new agent in a new tab of a workspace herdr has open. The form sends one
  // start request; the steps then come from the snapshot's starts, matched by
  // the job id kept with the draft, so a reload or reconnect shows the same job.
  import { onDestroy } from "svelte";
  import type { Agents } from "./lib/agents";
  import type { StartJob, StartKind, Workspace } from "./lib/protocol";
  import { LIST_HASH, agentHash } from "./lib/route";
  import type { Socket } from "./lib/socket";
  import {
    agentCount,
    defaultWorkspace,
    failedBeforeTab,
    isFinished,
    jobEnding,
    jobSteps,
    kindLabel,
    kindList,
    nameFromTask,
    nameProblem,
    JOB_ID_TTL_MS,
    newJobId,
    startErrorText,
    takenNames,
    taskProblem,
    uniqueName,
    workspacePath,
  } from "./lib/start";

  let {
    socket,
    loaded,
    reader,
    agents,
    workspaces,
    kinds,
    starts,
    lastWorkspace,
  }: {
    socket: Socket;
    /** A snapshot has arrived, so role, workspaces and starts are the server's. */
    loaded: boolean;
    reader: boolean;
    agents: Agents;
    /** In herdr's order. */
    workspaces: Workspace[];
    kinds: StartKind[];
    starts: StartJob[];
    /** The workspace of the agent last opened, the default "Where". */
    lastWorkspace: string | null;
  } = $props();

  interface Draft {
    task: string;
    workspace_id: string | null;
    kind: string | null;
    /** The typed name, used once name_edited is set; until then the name follows the task. */
    name: string;
    name_edited: boolean;
    /** Set when Start agent is tapped and kept until that job is over, so a retry resends it. */
    job_id: string | null;
    /** When job_id was made, in ms since the epoch. */
    job_at: number | null;
    /** The server took job_id (an ok ack), so the start went, whatever the snapshot says. */
    acked: boolean;
  }

  const EMPTY: Draft = { task: "", workspace_id: null, kind: null, name: "", name_edited: false, job_id: null, job_at: null, acked: false };
  const NO_JOB = { job_id: null, job_at: null, acked: false } as const;
  // Kept in localStorage, not the composer's sessionStorage: a job id has to
  // outlive the installed app being closed mid-start.
  const DRAFT_KEY = "herdr-pocket:start-draft";

  function readDraft(): Draft {
    try {
      const raw = localStorage.getItem(DRAFT_KEY);
      if (!raw) return { ...EMPTY };
      const d = JSON.parse(raw) as Partial<Draft>;
      const str = (v: unknown) => (typeof v === "string" ? v : null);
      return {
        task: str(d.task) ?? "",
        workspace_id: str(d.workspace_id),
        kind: str(d.kind),
        name: str(d.name) ?? "",
        name_edited: d.name_edited === true,
        job_id: str(d.job_id),
        job_at: typeof d.job_at === "number" ? d.job_at : null,
        acked: d.acked === true,
      };
    } catch {
      return { ...EMPTY };
    }
  }

  function saveDraft(d: Draft) {
    try {
      if (d.task || d.name_edited || d.job_id) localStorage.setItem(DRAFT_KEY, JSON.stringify(d));
      else localStorage.removeItem(DRAFT_KEY);
    } catch {
      // Storage blocked: the draft just isn't kept.
    }
  }

  let draft = $state<Draft>(readDraft());
  $effect(() => saveDraft($state.snapshot(draft)));

  const kindsShown = $derived(kindList(kinds));
  const workspaceId = $derived(defaultWorkspace(workspaces, draft.workspace_id, lastWorkspace));
  const kind = $derived(kindsShown.some((k) => k.kind === draft.kind) ? draft.kind! : (kindsShown[0]?.kind ?? null));
  const taken = $derived(takenNames(Object.values(agents), starts));
  const name = $derived(draft.name_edited ? draft.name : uniqueName(nameFromTask(draft.task), taken));
  const nameHint = $derived(draft.name_edited ? nameProblem(draft.name, taken) : null);

  // The start request is in flight.
  let sending = $state(false);
  // The job id the server took; its job may not be in starts yet.
  let accepted = $state<string | null>(null);
  let formError = $state<string | null>(null);
  let nameError = $state<string | null>(null);
  // The last copy of this draft's job, kept when it ages out of starts so a finished screen stays.
  let held = $state<StartJob | null>(null);

  $effect(() => {
    const id = draft.job_id;
    const found = id ? starts.find((j) => j.id === id) : undefined;
    if (found) held = found;
  });

  const job = $derived.by((): StartJob | null => {
    if (!draft.job_id) return null;
    const live = starts.find((j) => j.id === draft.job_id);
    if (live) return live;
    if (held?.id === draft.job_id) return held;
    // Accepted but not in a patch yet: show the first step under way.
    if (accepted === draft.job_id && workspaceId && kind)
      return { id: draft.job_id, by: "", workspace_id: workspaceId, kind, name, state: "running", began: new Date().toISOString() };
    return null;
  });

  // Ended before a tab opened: nothing to close, so the task is back in the
  // field with the reason, and the next try is a new job.
  $effect(() => {
    if (!job || !failedBeforeTab(job)) return;
    const ending = jobEnding(job, kindLabel(job.kind, kinds));
    formError = ending ? `${ending.title}. ${ending.body}` : null;
    if (job.error === "agent_name_taken" || job.error === "invalid_agent_name") nameError = formError;
    Object.assign(draft, NO_JOB);
    accepted = null;
  });

  // A job id the server doesn't hold. With nothing typed, start over. With the
  // task still here, the server may have run that job and since forgotten it
  // (jobs live in memory, an hour at most, 20 finished): if it took the id,
  // or the id is older than the server keeps a job, resending could start a
  // second agent, so the id is dropped and the person told. Otherwise the
  // request never arrived, and a resend under the same id is safe.
  $effect(() => {
    if (!loaded || !draft.job_id || job) return;
    if (!draft.task) {
      Object.assign(draft, NO_JOB);
      return;
    }
    if (draft.acked || Date.now() - (draft.job_at ?? 0) > JOB_ID_TTL_MS) {
      Object.assign(draft, NO_JOB);
      formError = "Your last start may already have gone. Check Agents before starting it again.";
    }
  });

  // Sent or lined up: the task has gone, so a reload after the job ages out shows a fresh form.
  $effect(() => {
    if (job && (job.state === "done" || job.state === "asks_first") && draft.task) draft.task = "";
  });

  // Leaving a finished job: done or asks first starts the next one fresh; a
  // failure keeps the task for another try under a new job.
  function settle() {
    if (!job || !isFinished(job)) return;
    draft = job.state === "done" || job.state === "asks_first" ? { ...EMPTY } : { ...draft, ...NO_JOB };
    held = null;
    accepted = null;
    // Saved here too: on leaving, the effect that saves is already gone.
    saveDraft($state.snapshot(draft));
  }
  onDestroy(settle);

  function retry() {
    settle();
    formError = null;
    nameError = null;
  }

  async function start(e: SubmitEvent) {
    e.preventDefault();
    if (sending) return;
    formError = taskProblem(draft.task);
    nameError = nameProblem(name, taken);
    if (formError || nameError || !workspaceId || !kind) return;
    // A retry after a lost or unanswered request resends the same id; the server
    // returns that job instead of opening a second tab.
    const jobId = draft.job_id ?? newJobId();
    if (jobId !== draft.job_id) Object.assign(draft, { job_id: jobId, job_at: Date.now(), acked: false });
    sending = true;
    const r = await socket.request({
      type: "start",
      job_id: jobId,
      workspace_id: workspaceId,
      kind,
      name,
      task: draft.task.trim(),
    });
    sending = false;
    if (r.ok) {
      // Unless the job already ended and the form moved on to a new one.
      if (draft.job_id === jobId) {
        accepted = jobId;
        draft.acked = true;
      }
      return;
    }
    if (r.error.code === "name_taken" || r.error.code === "invalid_name") nameError = startErrorText(r.error);
    else formError = startErrorText(r.error);
  }

  function editName(e: Event & { currentTarget: HTMLInputElement }) {
    draft.name = e.currentTarget.value;
    draft.name_edited = true;
    nameError = null;
  }

  const shownJob = $derived(job);
  const wsLabel = $derived(workspaces.find((w) => w.workspace_id === shownJob?.workspace_id)?.label ?? shownJob?.workspace_id ?? "");
  const shownKind = $derived(shownJob ? kindLabel(shownJob.kind, kinds) : "");
  const steps = $derived(shownJob ? jobSteps(shownJob, wsLabel, shownKind) : []);
  const ending = $derived(shownJob ? jobEnding(shownJob, shownKind) : null);
  // Open it: once the agent is in the list (asks first lands on its question).
  const openPane = $derived(shownJob?.pane_id && agents[shownJob.pane_id] ? shownJob.pane_id : null);
  const failed = $derived(!!shownJob && isFinished(shownJob) && shownJob.state !== "done" && shownJob.state !== "asks_first");
</script>

<div class="view">
  <nav>
    {#if shownJob}
      <a href={LIST_HASH} class="back">
        <svg width="10" height="17" viewBox="0 0 10 17" aria-hidden="true"><path d="M8.5 1.5L2 8.5l6.5 7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
        Agents
      </a>
    {:else}
      <a href={LIST_HASH} class="cancel">Cancel</a>
    {/if}
    <span class="title">New task</span>
    <span></span>
  </nav>

  {#if !loaded}
    <p class="note-page">Connecting…</p>
  {:else if reader}
    <p class="note-page">View only. You can see the agents but not start one.</p>
  {:else if shownJob}
    <div class="body">
      <header class="job">
        <h1>{shownJob.name}</h1>
        <span class="job-sub">{wsLabel} · {shownKind} · new tab</span>
      </header>
      <ol class="steps" aria-live="polite">
        {#each steps as s (s.step)}
          <li class={["step", s.mark]}>
            <span class="mark">
              {#if s.mark === "ok"}
                <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" stroke="#8e8e93" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
              {:else if s.mark === "now"}
                <svg class="spin" width="16" height="16" viewBox="0 0 18 18" aria-hidden="true"><circle cx="9" cy="9" r="7" stroke="#3a3a3c" stroke-width="2" fill="none"></circle><path d="M9 2a7 7 0 0 1 7 7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
              {:else if s.mark === "ask"}
                <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M6 6a2 2 0 1 1 2.8 1.8c-.5.3-.8.7-.8 1.3V10" stroke="#ff9f0a" stroke-width="1.8" fill="none" stroke-linecap="round"></path><circle cx="8" cy="12.6" r="1" fill="#ff9f0a"></circle></svg>
              {:else if s.mark === "fail"}
                <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M4.5 4.5l7 7M11.5 4.5l-7 7" stroke="#ff453a" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
              {:else}
                <span class="todo-dot"></span>
              {/if}
            </span>
            <span class="step-text">{s.text}</span>
          </li>
        {/each}
      </ol>
      {#if ending}
        <section class={["ending", ending.tone]} role={ending.tone === "fail" ? "alert" : "status"}>
          <span class="ending-title">{ending.title}</span>
          <span class="ending-body">{ending.body}</span>
        </section>
      {/if}
    </div>
    <footer class="pair">
      <a href={LIST_HASH} class="btn secondary">Back to Agents</a>
      {#if failed && !openPane}
        <button type="button" class="btn primary" onclick={retry}>Try again</button>
      {:else if openPane}
        <a href={agentHash(openPane)} class="btn primary">Open it</a>
      {:else}
        <span class="btn primary waiting" aria-disabled="true">Open it</span>
      {/if}
    </footer>
  {:else if !workspaces.length || !kindsShown.length}
    <p class="note-page">
      {!workspaces.length ? "herdr has no workspace open to start an agent in." : "This computer doesn’t offer any agent to start."}
    </p>
  {:else}
    <form class="form" onsubmit={start} novalidate>
      <div class="body">
        <section class="field-group" aria-label="Task">
          <label for="task" class="h">What should it do?</label>
          <textarea id="task" rows="4" placeholder="Describe the task" bind:value={draft.task} oninput={() => (formError = null)}></textarea>
        </section>

        <section class="field-group" aria-labelledby="where">
          <h2 id="where" class="h">Where</h2>
          <div role="radiogroup" aria-labelledby="where" class="group">
            {#each workspaces as w (w.workspace_id)}
              {@const on = w.workspace_id === workspaceId}
              <button type="button" role="radio" aria-checked={on} class="ws" onclick={() => {
                  draft.workspace_id = w.workspace_id;
                  formError = null;
                }}>
                <span class="ws-text">
                  <span class="ws-label">{w.label}</span>
                  <span class={["ws-path", !w.path && "current"]}>{workspacePath(w)}</span>
                </span>
                <span class="ws-count">{agentCount(w.agent_count)}</span>
                <span class="check">
                  {#if on}
                    <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
                  {/if}
                </span>
              </button>
            {/each}
          </div>
          <p class="sub">Opens a new tab; your desktop layout isn’t touched.</p>
        </section>

        <section class="group agent" aria-label="Agent">
          <label class="row">
            <span class="row-label">Agent</span>
            <span class="pick">
              <select id="kind" bind:value={() => kind ?? "", (v) => (draft.kind = v)}>
                {#each kindsShown as k (k.kind)}
                  <option value={k.kind}>{k.label}</option>
                {/each}
              </select>
              <svg width="8" height="14" viewBox="0 0 8 14" aria-hidden="true"><path d="M1.5 1.5L6.5 7l-5 5.5" stroke="#48484a" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
            </span>
          </label>
          <div class="row divided">
            <label for="name" class="row-label">Name</label>
            <input
              id="name"
              type="text"
              value={name}
              oninput={editName}
              autocapitalize="none"
              autocomplete="off"
              spellcheck="false"
              maxlength="32"
              aria-describedby="name-hint"
              aria-invalid={!!(nameError ?? nameHint)}
            />
          </div>
        </section>
        {#if nameError ?? nameHint}
          <p id="name-hint" class="hint" role="alert">{nameError ?? nameHint}</p>
        {/if}
      </div>

      <footer class="one">
        {#if formError}
          <p class="error" role="alert">{formError}</p>
        {/if}
        <button type="submit" class="btn primary" disabled={sending}>{sending ? "Starting…" : "Start agent"}</button>
        <span class="cta-note">Starts with no extra flags. Close it from the desktop.</span>
      </footer>
    </form>
  {/if}
</div>

<style>
  .view {
    height: 100dvh;
    box-sizing: border-box;
    display: flex;
    flex-direction: column;
  }
  nav {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    align-items: center;
    padding: 8px 8px 4px;
  }
  .back,
  .cancel {
    justify-self: start;
    min-height: 44px;
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 0 8px;
    color: #e6fb04;
    font-size: 16px;
    font-weight: 500;
  }
  .back:hover,
  .cancel:hover {
    color: #e6fb04;
  }
  .title {
    text-align: center;
    font-size: 16px;
    font-weight: 600;
  }
  .note-page {
    margin: 24px 20px;
    font-size: 15px;
    line-height: 1.4;
    color: #a1a1a6;
  }
  .form {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  .body {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    scrollbar-width: none;
    padding: 8px 16px 16px;
    display: flex;
    flex-direction: column;
    gap: 22px;
  }
  .body::-webkit-scrollbar {
    display: none;
  }
  .field-group {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .h {
    margin: 0 4px;
    font-size: 15px;
    font-weight: 600;
  }
  textarea {
    box-sizing: border-box;
    width: 100%;
    min-height: 112px;
    max-height: 260px;
    /* Grows with its text, then scrolls, as the composer does. */
    field-sizing: content;
    resize: none;
    border: 0;
    outline: 0;
    border-radius: 14px;
    background: #1c1c1e;
    color: #f5f5f7;
    padding: 14px 16px;
    margin: 0;
    font-family: Inter, sans-serif;
    font-size: 16px;
    line-height: 1.45;
  }
  textarea::placeholder {
    color: #8e8e93;
  }
  .group {
    background: #1c1c1e;
    border-radius: 14px;
    overflow: hidden;
  }
  .ws {
    width: 100%;
    min-height: 54px;
    border: 0;
    background: transparent;
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 14px;
    text-align: left;
    cursor: pointer;
    color: #f5f5f7;
    font-family: Inter, sans-serif;
  }
  .ws + .ws {
    border-top: 0.5px solid #38383a;
  }
  .ws-text {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .ws-label {
    font-size: 16px;
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .ws-path {
    font-family: "JetBrains Mono", monospace;
    font-size: 12px;
    color: #8e8e93;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .ws-path.current {
    font-family: Inter, sans-serif;
    font-size: 13px;
  }
  .ws-count {
    flex: none;
    font-size: 13px;
    color: #8e8e93;
  }
  .check {
    flex: none;
    width: 16px;
    height: 16px;
  }
  .sub {
    margin: 0 4px;
    font-size: 13px;
    color: #8e8e93;
  }
  .row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    min-height: 50px;
    padding: 0 16px;
  }
  .divided {
    border-top: 0.5px solid #38383a;
  }
  .row-label {
    flex: 0 0 auto;
    font-size: 16px;
  }
  .pick {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  select {
    appearance: none;
    min-height: 44px;
    min-width: 0;
    border: 0;
    outline: 0;
    background: transparent;
    color: #a1a1a6;
    text-align: right;
    font-family: Inter, sans-serif;
    font-size: 15px;
    cursor: pointer;
  }
  select:focus-visible,
  input:focus-visible {
    color: #f5f5f7;
  }
  option {
    background: #1c1c1e;
    color: #f5f5f7;
  }
  input {
    flex: 1;
    min-width: 0;
    min-height: 44px;
    border: 0;
    outline: 0;
    background: transparent;
    color: #f5f5f7;
    text-align: right;
    font-family: "JetBrains Mono", monospace;
    font-size: 14px;
  }
  input[aria-invalid="true"] {
    color: #ff453a;
  }
  .hint {
    margin: -14px 4px 0;
    font-size: 13px;
    color: #ff453a;
  }
  footer {
    padding: 10px 12px calc(22px + env(safe-area-inset-bottom, 0px));
    border-top: 0.5px solid #2c2c2e;
    box-sizing: border-box;
  }
  .one {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .pair {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 8px;
  }
  .btn {
    min-height: 50px;
    border-radius: 14px;
    border: 0;
    font-family: Inter, sans-serif;
    font-size: 16px;
    font-weight: 600;
    display: flex;
    align-items: center;
    justify-content: center;
    cursor: pointer;
  }
  .primary {
    background: #e6fb04;
    color: #000;
  }
  .primary:hover {
    color: #000;
  }
  .primary:disabled,
  .waiting {
    opacity: 0.45;
    cursor: default;
  }
  .secondary {
    background: #2c2c2e;
    color: #f5f5f7;
  }
  .cta-note {
    font-size: 12px;
    color: #8e8e93;
    text-align: center;
  }
  .error {
    margin: 0 4px;
    font-size: 14px;
    line-height: 1.4;
    color: #ff453a;
  }
  .job {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 0 4px;
  }
  h1 {
    margin: 0;
    font-size: 26px;
    font-weight: 700;
    letter-spacing: -0.015em;
    overflow-wrap: anywhere;
  }
  .job-sub {
    font-size: 15px;
    color: #8e8e93;
  }
  .steps {
    list-style: none;
    margin: 0;
    padding: 0;
    background: #1c1c1e;
    border-radius: 14px;
    overflow: hidden;
  }
  .step {
    display: flex;
    gap: 12px;
    padding: 14px 16px;
    align-items: center;
    font-size: 15px;
    color: #f5f5f7;
  }
  .step + .step {
    border-top: 0.5px solid #38383a;
  }
  .step.todo {
    color: #8e8e93;
  }
  .step.ask {
    color: #ffb340;
  }
  .step.fail {
    color: #ff6961;
  }
  .mark {
    width: 22px;
    flex: 0 0 auto;
    display: flex;
    justify-content: center;
  }
  .todo-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: #48484a;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .spin {
    animation: spin 1.1s linear infinite;
    transform-origin: 50% 50%;
  }
  @media (prefers-reduced-motion: reduce) {
    .spin {
      animation: none;
    }
  }
  .ending {
    background: #1c1c1e;
    border-radius: 14px;
    padding: 14px 16px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .ending-title {
    font-size: 15px;
    font-weight: 600;
  }
  .ending.ask .ending-title {
    color: #ffb340;
  }
  .ending.fail .ending-title {
    color: #ff6961;
  }
  .ending-body {
    font-size: 14px;
    line-height: 1.4;
    color: #a1a1a6;
  }
</style>
