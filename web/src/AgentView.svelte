<script lang="ts">
  // The agent view. While a question is open it becomes the answer screen:
  // the header and WHAT LED HERE above, the sheet in place of the composer.
  // Otherwise it answers "what's next": the last message in full, the work
  // lined up for when it's done, the Next list, and the terminal behind the
  // "…" menu.
  import { type Agents, nextWaiting, waitingPosition } from "./lib/agents";
  import type { Detail, Question, QuestionOption, QueueItem, RequestBody } from "./lib/protocol";
  import { LIST_HASH, agentHash } from "./lib/route";
  import type { Link, RequestResult, Socket } from "./lib/socket";
  import {
    cardLabel,
    composerPlaceholder,
    contextLine,
    errorText,
    followLabel,
    isAsking,
    linkWords,
    lineUpLabel,
    linedUpLine,
    prLabel,
    removeErrorText,
    sentLabel,
    sentSub,
    statusLabel,
  } from "./lib/words";
  import AnswerSheet from "./AnswerSheet.svelte";
  import ClearSheet from "./ClearSheet.svelte";
  import LastMessage from "./LastMessage.svelte";
  import { splitQuestion } from "./lib/message";
  import QuickKeys from "./QuickKeys.svelte";

  let {
    paneId,
    agents,
    detail,
    output,
    connected,
    link,
    lastUpdate,
    onretry,
    loaded,
    now,
    socket,
    lined,
    reader,
  }: {
    paneId: string;
    agents: Agents;
    detail: Detail | null;
    output: string | null;
    connected: boolean;
    /** The connection to the server, and when its last frame came. */
    link: Link;
    lastUpdate: number | null;
    /** Try to connect now. */
    onretry: () => void;
    /** A snapshot has arrived since the page loaded. */
    loaded: boolean;
    now: number;
    socket: Socket;
    /** This agent's lined-up items, finished first, then waiting in sending order. */
    lined: QueueItem[];
    /** View only: no composer, no keys, no answer buttons. */
    reader: boolean;
  } = $props();

  const agent = $derived(agents[paneId] ?? null);
  const question = $derived(detail?.question ?? null);
  const terminalSource = $derived(detail?.source === "terminal");

  // "Stay here" hides the sheet for the question it was on; a new question id shows it again.
  let dismissed = $state<string | null>(null);
  // After a successful answer: the confirmation title, the question it answered, and the
  // "N of M waiting" from when it was sent (the agent leaves blocked before the ack).
  let sent = $state<{ label: string; sub: string; question: Question; position: typeof position } | null>(null);
  let answering = $state(false);
  let sheetError = $state<string | null>(null);
  // A step of a multi-question ask was answered: the sheet stays on it, busy,
  // until the screen shows the next step. With the own-answer box focused
  // herdr may briefly call the agent idle, so the question can vanish in
  // between; stepping covers that gap, for at most STEP_WAIT.
  let stepping = $state<Question | null>(null);
  const STEP_WAIT = 8000;
  $effect(() => {
    if (stepping && question && question.id !== stepping.id) stepping = null;
  });
  $effect(() => {
    if (!stepping) return;
    const t = setTimeout(() => (stepping = null), STEP_WAIT);
    return () => clearTimeout(t);
  });

  const shown = $derived(
    sent?.question ??
      (question && question.id !== dismissed && question.id !== stepping?.id ? question : null) ??
      stepping,
  );
  const position = $derived(waitingPosition(agents, paneId));
  const shownPosition = $derived(sent ? sent.position : position);

  // While it's working a message is lined up for when it's done, and followed in a
  // bubble over the field that offers Send now; idle or done, messages go now.
  const linesUp = $derived(!reader && agent?.status === "working");
  const waiting = $derived(lined.filter((it) => it.state === "waiting"));
  const nextId = $derived(waiting[0]?.id ?? null);
  let linedEl = $state<HTMLElement | null>(null);

  const status = $derived(agent?.status ?? "unknown");
  const asking = $derived(!!detail && isAsking(status, detail.asks));
  // The top card: the two-sentence now while a transcript agent works, else the
  // whole last message; a terminal agent always shows its screen's last lines.
  const cardText = $derived.by(() => {
    if (!detail) return null;
    if (status === "working" && !terminalSource) return detail.now ? { text: detail.now, whole: false } : null;
    if (detail.last_message) return { text: detail.last_message, whole: true };
    return detail.now ? { text: detail.now, whole: false } : null;
  });
  // Start fresh: a finished or idle transcript agent, controllers only.
  const canClear = $derived(!reader && !terminalSource && (status === "done" || status === "idle"));
  let confirmClear = $state(false);
  // The sheet goes if the agent stops being done or idle while it's open.
  $effect(() => {
    if (!canClear) confirmClear = false;
  });
  // A reply button's send is in flight; the buttons wait for its ack.
  let replying = $state(false);
  // The message a reply answered. Its buttons stay gone until the detail moves on,
  // as asks and the status lag the ack by a poll or more.
  let repliedTo = $state<string | null>(null);
  $effect(() => {
    if (status === "working" || (detail?.last_message ?? null) !== repliedTo) repliedTo = null;
  });

  // The draft survives a reload (the app reloads itself after an update) and a
  // trip to another agent, per agent, for this tab only.
  const draftKey = $derived(`herdr-pocket:draft:${paneId}`);
  let message = $state(readDraft());
  function readDraft(): string {
    try {
      return sessionStorage.getItem(draftKey) ?? "";
    } catch {
      return "";
    }
  }
  $effect(() => {
    try {
      if (message) sessionStorage.setItem(draftKey, message);
      else sessionStorage.removeItem(draftKey);
    } catch {
      // Storage blocked: the draft just isn't kept.
    }
  });
  // The message just sent, shown above the field until it is acked and a moment after.
  let echo = $state<{ text: string; sent: boolean } | null>(null);
  let echoTimer: ReturnType<typeof setTimeout> | undefined;
  // Sends go out one after another, in the order typed; a new one never waits on the field.
  let turns: Promise<unknown> = Promise.resolve();
  // The prompt whose ack the echo waits on.
  let echoTurn: Promise<unknown> | null = null;
  let composerError = $state<string | null>(null);
  // The last message lined up from the field: its text, its item once the ack names
  // it, and whether the list has shown it yet (the ack can beat the list's patch).
  let followed = $state<{ text: string; id: string | null; seen: boolean } | null>(null);
  const followedItem = $derived(followed?.id ? lined.find((it) => it.id === followed?.id) : undefined);
  const followedAhead = $derived.by(() => {
    const it = followedItem;
    return it && it.state === "waiting" ? waiting.findIndex((w) => w.id === it.id) : 0;
  });
  $effect(() => {
    if (!followed?.id) return;
    if (followedItem) followed.seen = true;
    // Removed, or aged out of the list after it was shown: nothing left to follow.
    else if (followed.seen) followed = null;
  });
  // Picked up: say so for a moment, then step aside.
  $effect(() => {
    if (followedItem?.state !== "picked_up") return;
    const t = setTimeout(() => (followed = null), 4000);
    return () => clearTimeout(t);
  });
  let menuOpen = $state(false);
  // Steps show the latest few, newest first; the whole list is one tap away.
  const STEPS_SHOWN = 3;
  let allSteps = $state(false);
  // Newest first, so a glance reads what it did last.
  const shownSteps = $derived.by(() => {
    const steps = detail?.steps ?? [];
    return (allSteps ? steps : steps.slice(-STEPS_SHOWN)).slice().reverse();
  });
  // The full-screen terminal, opened from the "…" menu. A question closes it, so
  // the answer sheet is never under it.
  let terminalOpen = $state(false);
  $effect(() => {
    if (shown) terminalOpen = false;
  });

  // Tell the server whether the terminal is on screen, so it polls the screen only then
  // (a terminal-source agent's screen is polled anyway); re-sent on every (re)connect.
  $effect(() => {
    if (!connected) return;
    socket.watch({ type: "terminal", pane_id: paneId, shown: terminalOpen });
  });

  function request(body: RequestBody): Promise<RequestResult> {
    return socket.request(body);
  }

  async function sendKeys(keys: string[]): Promise<RequestResult> {
    const r = await request({ type: "keys", pane_id: paneId, keys });
    composerError = r.ok ? null : errorText(r.error);
    return r;
  }

  async function sendKeysFromSheet(key: string) {
    sheetError = null;
    const r = await request({ type: "keys", pane_id: paneId, keys: [key] });
    if (!r.ok) sheetError = errorText(r.error);
  }

  // The field clears at once and the message shows as "Sending…" until the
  // ack, so a send never waits on the round trip. A lined-up message has no
  // echo: it shows in the Lined up list once the server has it.
  async function sendPrompt(e?: SubmitEvent) {
    e?.preventDefault();
    const text = message.trim();
    if (!text) return;
    message = "";
    await deliver(text, linesUp, true);
  }

  // A reply button or /clear: sent now, with the same echo, leaving a typed draft alone.
  async function reply(text: string) {
    if (replying) return;
    replying = true;
    const to = detail?.last_message ?? null;
    if (await deliver(text, false, false)) repliedTo = to;
    replying = false;
  }

  // Not through reply(): a confirmed /clear goes even while a reply is in flight.
  function sendClear() {
    confirmClear = false;
    void deliver("/clear", false, false);
  }

  // Sends text, or lines it up; restore puts a failed message back in the field.
  async function deliver(text: string, lineUp: boolean, restore: boolean): Promise<boolean> {
    composerError = null;
    const body: RequestBody = lineUp
      ? { type: "queue.add", target: { pane_id: paneId }, text }
      : { type: "prompt", pane_id: paneId, text };
    const turn = turns.then(() => request(body));
    turns = turn;
    const follow = lineUp ? { text, id: null, seen: false } : null;
    if (follow) followed = follow;
    else {
      clearTimeout(echoTimer);
      echo = { text, sent: false };
      echoTurn = turn;
    }
    const r = await turn;
    if (follow) {
      // Only if no later line-up took the bubble meanwhile.
      if (followed?.text === follow.text && followed.id === null) followed = r.ok && r.itemId ? { ...follow, id: r.itemId } : null;
    }
    if (r.ok) {
      if (lineUp || echoTurn !== turn) return true; // a later send owns the echo now
      echo = { text, sent: true };
      echoTimer = setTimeout(() => (echo = null), 4000);
      return true;
    }
    // The message goes back in the field, ahead of anything typed since, so
    // it can be sent after answering.
    if (echoTurn === turn) echo = null;
    if (restore) message = message ? `${text}\n${message}` : text;
    if (r.error.code === "agent_blocked") dismissed = null;
    composerError = errorText(r.error);
    return false;
  }

  // Enter sends; Shift+Enter (or an IME composing) adds a new line.
  function onComposeKey(e: KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      void sendPrompt();
    }
  }

  // The followed message goes now instead: taken off the list first, so it can't go twice.
  async function sendNow() {
    const it = followedItem;
    if (!it || it.state !== "waiting") return;
    composerError = null;
    const r = await request({ type: "queue.remove", item_id: it.id });
    if (!r.ok) {
      composerError = r.error.code === "not_waiting" ? "It had already gone to the agent." : removeErrorText(r.error);
      return;
    }
    followed = null;
    await deliver(it.text, false, false);
  }

  async function remove(item: QueueItem) {
    composerError = null;
    const r = await request({ type: "queue.remove", item_id: item.id });
    if (!r.ok) composerError = removeErrorText(r.error);
  }

  async function answer(q: Question, option: QuestionOption, note?: string) {
    if (answering) return;
    answering = true;
    sheetError = null;
    const at = position;
    const r = await request({
      type: "answer",
      pane_id: paneId,
      question_id: q.id,
      option: option.key,
      ...(note ? { note } : {}),
    });
    answering = false;
    if (!r.ok) sheetError = errorText(r.error);
    else if ((q.steps?.length ?? 0) > 1 && !q.review) stepping = q;
    else sent = { label: sentLabel(option, note), sub: sentSub(option), question: q, position: at };
  }

  function stay() {
    dismissed = sent?.question.id ?? shown?.id ?? null;
    sent = null;
    sheetError = null;
    composerError = null;
  }

  function next() {
    const id = nextWaiting(agents, paneId);
    location.hash = id ? agentHash(id) : LIST_HASH;
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key !== "Escape") return;
    if (terminalOpen && !menuOpen) terminalOpen = false;
    menuOpen = false;
  }

  function openTerminal() {
    menuOpen = false;
    terminalOpen = true;
  }
</script>

<svelte:window onkeydown={onKeydown} />

<div class="view">
  <nav>
    <a href={LIST_HASH} class="back">
      <svg width="10" height="17" viewBox="0 0 10 17" aria-hidden="true"><path d="M8.5 1.5L2 8.5l6.5 7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
      Agents
    </a>
    {#if shown}
      {#if shownPosition}
        <span class="waiting">{shownPosition.index} of {shownPosition.total} waiting</span>
      {/if}
    {:else}
      <span class="nav-actions">
      {#if !reader && agent?.status === "working"}
        <button type="button" class="stop" onclick={() => sendKeys(["esc"])}>
          <svg width="11" height="11" viewBox="0 0 12 12" aria-hidden="true"><rect x="1.5" y="1.5" width="9" height="9" rx="2" fill="#f5f5f7"></rect></svg>
          Stop
        </button>
      {/if}
      <button
        type="button"
        class="more"
        aria-label="More: terminal, keys, agent details"
        aria-expanded={menuOpen}
        aria-controls="keys"
        onclick={() => (menuOpen = !menuOpen)}
      >
        <svg width="20" height="20" viewBox="0 0 20 20" aria-hidden="true"><circle cx="4" cy="10" r="1.7" fill="#e6fb04"></circle><circle cx="10" cy="10" r="1.7" fill="#e6fb04"></circle><circle cx="16" cy="10" r="1.7" fill="#e6fb04"></circle></svg>
      </button>
      </span>
    {/if}
  </nav>
  <!-- Not before the first snapshot: opening the app isn't an outage. -->
  {#if loaded && link.state !== "live"}
    {@const w = linkWords(link.state, link.retryAt, lastUpdate, now)}
    <div class={["link", link.state]} role="status">
      <span class="link-dot"></span>
      <span class="link-words">
        <span class="link-title">{w.title}</span>
        {#if w.detail}<span class="link-detail">{w.detail}</span>{/if}
      </span>
      {#if link.state === "down" || link.state === "offline"}
        <button type="button" class="link-retry" onclick={onretry}>Retry</button>
      {/if}
    </div>
  {/if}

  {#if menuOpen && !shown}
    <button type="button" class="backdrop" aria-label="Close menu" onclick={() => (menuOpen = false)}></button>
    <div id="keys" class="menu" role="group" aria-label="More">
      <button type="button" class="show-term" onclick={openTerminal}>
        <span>Show terminal</span>
        <span class="hint">exactly as on the desktop</span>
      </button>
      {#if agent}
        <dl class="info">
          <div><dt>Agent</dt><dd>{detail?.agent_label ?? agent.agent}</dd></div>
          <div><dt>Pane</dt><dd>{agent.pane_id}</dd></div>
          {#if detail && detail.context_pct !== null}
            <div><dt>Context used</dt><dd>{detail.context_pct}%</dd></div>
          {/if}
        </dl>
      {/if}
      {#if !reader}
        <span class="label">SEND A KEY</span>
        <QuickKeys onkey={(key) => sendKeys([key])} />
      {/if}
    </div>
  {/if}

  <div class={["scroll", shown && "answer"]}>
    {#if !agent}
      <p class="gone">{connected ? "This agent isn't running any more." : "Reconnecting…"}</p>
    {:else}
      <header>
        <h1>{agent.name}</h1>
        <div class="status">
          {#if sent}
            <span class="dot again"></span>
            <span class="state">Working again</span>
          {:else if shown}
            <span class="dot blocked"></span>
            <span class="state blocked">{statusLabel("blocked", detail?.status_since ?? agent.changed_at, now)}</span>
          {:else}
            {#if agent.status === "working"}
              <svg class="spin" width="16" height="16" viewBox="0 0 18 18" aria-hidden="true"><circle cx="9" cy="9" r="7" stroke="#3a3a3c" stroke-width="2" fill="none"></circle><path d="M9 2a7 7 0 0 1 7 7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
            {:else if agent.status === "done"}
              <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" stroke="#64d2ff" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
            {:else if agent.status === "idle"}
              <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><rect x="3" y="3" width="3" height="8" rx="1" fill="#8e8e93"></rect><rect x="8" y="3" width="3" height="8" rx="1" fill="#8e8e93"></rect></svg>
            {:else}
              <span class={["dot", agent.status]}></span>
            {/if}
            <span class={["state", agent.status]}>{statusLabel(agent.status, detail?.status_since ?? agent.changed_at, now)}</span>
          {/if}
        </div>
        <div class="where">
          <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><path d="M1.5 3.5a1 1 0 0 1 1-1h3l1.5 1.5h4.5a1 1 0 0 1 1 1v5.5a1 1 0 0 1-1 1h-9a1 1 0 0 1-1-1z" stroke="#8e8e93" stroke-width="1.3" fill="none" stroke-linejoin="round"></path></svg>
          <span class="sr">Workspace</span>
          <span class="ws">{agent.workspace_label}</span>
        </div>
      </header>

      {#if shown}
        {#if shown.led_here}
          <section aria-label="What led here" class="card led">
            <span class="label">WHAT LED HERE</span>
            <p>{shown.led_here}</p>
          </section>
        {/if}
      {:else}
        {#if detail && cardText}
          {@const label = cardLabel(status, detail.asks)}
          <section aria-label={label} class="card top">
            <span class="label">{label}</span>
            {#if cardText.whole && asking && !terminalSource}
              {@const parts = splitQuestion(cardText.text)}
              {#if parts.body}
                <LastMessage text={parts.body} plain={false} />
              {/if}
              <div class="question"><LastMessage text={parts.question} plain={false} clamp={false} /></div>
            {:else if cardText.whole}
              <LastMessage text={cardText.text} plain={terminalSource} />
            {:else}
              <p>{cardText.text}</p>
            {/if}
            {#if asking && !reader && detail.last_message !== repliedTo}
              <div class="replies">
                {#each ["Yes", "No", "Not yet"] as word, i (word)}
                  <button type="button" class={["reply", i === 0 && "primary"]} disabled={replying} onclick={() => reply(word)}>{word}</button>
                {/each}
              </div>
            {/if}
          </section>
        {/if}

        {#if detail && ((status === "working" && waiting.length > 0) || detail.pr || canClear)}
          <section aria-label="Next" class="next-list">
            <h2>Next</h2>
            <div class="rows">
              {#if status === "working" && waiting.length > 0}
                <button type="button" class="action" onclick={() => linedEl?.scrollIntoView({ block: "start" })}>
                  <span class="glyph">
                    <svg width="14" height="14" viewBox="0 0 12 12" aria-hidden="true"><path d="M2 3h8M2 6h8M2 9h5" stroke="#e6fb04" stroke-width="1.4" fill="none" stroke-linecap="round"></path></svg>
                  </span>
                  <span class="words">
                    <span class="action-label">{linedUpLine(waiting.length)}</span>
                    <span class="action-sub">Next: {waiting[0]!.text}</span>
                  </span>
                  <svg class="chev" width="8" height="14" viewBox="0 0 8 14" aria-hidden="true"><path d="M1.5 1.5L6.5 7l-5 5.5" stroke="#48484a" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
                </button>
              {/if}
              {#if detail.pr}
                <div class="action info-row">
                  <span class="glyph">
                    <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><circle cx="4.5" cy="3.5" r="1.8" stroke="#e6fb04" stroke-width="1.4" fill="none"></circle><circle cx="4.5" cy="12.5" r="1.8" stroke="#e6fb04" stroke-width="1.4" fill="none"></circle><circle cx="11.5" cy="12.5" r="1.8" stroke="#e6fb04" stroke-width="1.4" fill="none"></circle><path d="M4.5 5.3v5.4M11.5 10.7V6.5a2 2 0 0 0-2-2H7.5" stroke="#e6fb04" stroke-width="1.4" fill="none" stroke-linecap="round"></path></svg>
                  </span>
                  <span class="words">
                    <span class="action-label">{prLabel(detail.pr)}</span>
                    <span class="action-sub">From its status line</span>
                  </span>
                </div>
              {/if}
              {#if canClear}
                {@const ctx = contextLine(detail.context_pct)}
                <button type="button" class="action" onclick={() => (confirmClear = true)}>
                  <span class="glyph">
                    <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M3 8a5 5 0 1 0 1.5-3.6M3 2.5v2.5h2.5" stroke="#e6fb04" stroke-width="1.5" fill="none" stroke-linecap="round" stroke-linejoin="round"></path></svg>
                  </span>
                  <span class="words">
                    <span class="action-label">Start fresh with /clear</span>
                    {#if ctx}
                      <span class="action-sub">{ctx}</span>
                    {/if}
                  </span>
                  <svg class="chev" width="8" height="14" viewBox="0 0 8 14" aria-hidden="true"><path d="M1.5 1.5L6.5 7l-5 5.5" stroke="#48484a" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
                </button>
              {/if}
            </div>
          </section>
        {/if}

        {#if lined.length > 0}
          <section aria-label="Lined up" class="lined" bind:this={linedEl}>
            <h2>Lined up<span class="sub">sent one at a time, when it’s done</span></h2>
            <ol>
              {#each lined as item (item.id)}
                <li>
                  <span class="item">
                    <span class={["item-text", item.state !== "waiting" && "finished"]}>{item.text}</span>
                    <span class={["item-meta", item.id === nextId && "next"]}>{lineUpLabel(item, item.id === nextId, now)}</span>
                  </span>
                  {#if !reader && item.state === "waiting"}
                    <button type="button" class="remove" aria-label="Remove from the list" onclick={() => remove(item)}>
                      <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><path d="M3 3l8 8M11 3l-8 8" stroke="#8e8e93" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
                    </button>
                  {/if}
                </li>
              {/each}
            </ol>
          </section>
        {/if}
      {/if}

      {#if !shown && detail}
        {#if !terminalSource && detail.steps.length > 0}
          <section aria-label="Steps" class="steps">
            <h2>Latest steps</h2>
            <ol>
              {#each shownSteps as step, i (i)}
                <li>
                  <span class="icon">
                    {#if step.kind === "you"}
                      <svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true"><circle cx="9" cy="6" r="3" stroke="#e6fb04" stroke-width="1.6" fill="none"></circle><path d="M3 16c.8-3 3.2-4.5 6-4.5s5.2 1.5 6 4.5" stroke="#e6fb04" stroke-width="1.6" fill="none" stroke-linecap="round"></path></svg>
                    {:else if step.kind === "ok"}
                      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" stroke="#8e8e93" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
                    {:else if step.kind === "fail"}
                      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M4.5 4.5l7 7M11.5 4.5l-7 7" stroke="#ff453a" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
                    {:else}
                      <svg class="spin" width="16" height="16" viewBox="0 0 18 18" aria-hidden="true"><circle cx="9" cy="9" r="7" stroke="#3a3a3c" stroke-width="2" fill="none"></circle><path d="M9 2a7 7 0 0 1 7 7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
                    {/if}
                  </span>
                  <span class="text">
                    <span class={["what", step.kind]}>{step.text}</span>
                    {#if step.meta}
                      <span class="meta">{step.meta}</span>
                    {/if}
                  </span>
                </li>
              {/each}
            </ol>
            {#if detail.steps.length > STEPS_SHOWN}
              <button type="button" class="more-steps" aria-expanded={allSteps} onclick={() => (allSteps = !allSteps)}>
                {allSteps ? "Show the latest" : `Show all ${detail.steps.length} steps`}
              </button>
            {/if}
          </section>
        {/if}
      {/if}

      {#if !shown}
        <p class="term-hint">The terminal is in the … menu.</p>
      {/if}
    {/if}
  </div>

  {#if shown}
    {#key shown.id}
      <AnswerSheet
        question={shown}
        {reader}
        sent={sent?.label ?? null}
        sentSub={sent?.sub ?? ""}
        busy={answering || stepping !== null}
        error={sheetError}
        onanswer={(option, note) => answer(shown, option, note)}
        onkey={sendKeysFromSheet}
        onstay={stay}
        onnext={next}
        hasNext={nextWaiting(agents, paneId) !== null}
      />
    {/key}
  {:else if agent && reader}
    <footer class="view-only">
      <p>View only. Ask the owner for control.</p>
    </footer>
  {:else if agent}
    <footer>
      {#if composerError}
        <p class="error" role="alert">{composerError}</p>
      {/if}
      {#if followed}
        <div class="echo" aria-live="polite">
          <span class="echo-bubble"><span class="echo-text">{followed.text}</span></span>
          <span class="echo-state follow">
            {followLabel(followedItem, followedAhead, now)}
            {#if followedItem?.state === "waiting"}
              <button type="button" class="now" onpointerdown={(e) => e.preventDefault()} onclick={sendNow}>Send now</button>
            {/if}
          </span>
        </div>
      {/if}
      {#if echo}
        <p class="echo" aria-live="polite">
          <span class="echo-bubble"><span class="echo-text">{echo.text}</span></span>
          <span class="echo-state">{echo.sent ? "Sent" : "Sending…"}</span>
        </p>
      {/if}
      <div class="bar-row">
        <form class="compose" onsubmit={sendPrompt}>
          <label for="msg" class="sr">Message this agent</label>
          <div class="field">
            <textarea
              id="msg"
              rows="1"
              placeholder={loaded && (link.state === "down" || link.state === "offline" || link.state === "connecting")
                ? "Not connected · send once it’s back"
                : composerPlaceholder(status, detail?.asks ?? false, linesUp)}
              autocomplete="off"
              enterkeyhint="send"
              bind:value={message}
              onkeydown={onComposeKey}
            ></textarea>
            <!-- preventDefault on pointerdown keeps focus in the field, so the keyboard stays up. -->
            <button
              type="submit"
              class="send"
              aria-label={linesUp ? "Send when it’s done" : "Send message"}
              onpointerdown={(e) => e.preventDefault()}
            >
              <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><path d="M7 11.5V2.5M3 6.5l4-4 4 4" stroke="#000" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
            </button>
          </div>
        </form>
      </div>
    </footer>
  {/if}

  {#if confirmClear && agent}
    <ClearSheet name={agent.name} onconfirm={sendClear} oncancel={() => (confirmClear = false)} />
  {/if}

  {#if terminalOpen}
    <section class="term" aria-label="Terminal">
      <div class="term-bar">
        <span class="term-title">Terminal<span class="hint">exactly as on the desktop</span></span>
        <button type="button" class="done-btn" onclick={() => (terminalOpen = false)} {@attach (node) => node.focus()}>Done</button>
      </div>
      <pre>{output ?? "Waiting for the screen…"}</pre>
    </section>
  {/if}
</div>

<style>
  .view {
    height: 100dvh;
    box-sizing: border-box;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    position: relative;
  }
  /* Over the page under the nav, so it coming and going never moves what's read. */
  .link {
    position: absolute;
    top: 52px;
    left: 16px;
    right: 16px;
    /* Under the "…" menu, which comes later in the page at the same level. */
    z-index: 1;
    margin: 0;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.5);
    padding: 8px 12px;
    border-radius: 10px;
    background: #2c2c2e;
    font-size: 14px;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .link-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    flex: none;
  }
  .link.slow .link-dot,
  .link.connecting .link-dot {
    background: #ff9f0a;
  }
  .link.down .link-dot,
  .link.offline .link-dot {
    background: #ff453a;
  }
  .link-words {
    display: flex;
    flex-direction: column;
    min-width: 0;
    flex: 1;
  }
  .link-title {
    font-weight: 600;
  }
  .link-detail {
    font-size: 13px;
    color: #a1a1a6;
  }
  .link-retry {
    flex: none;
    min-height: 36px;
    padding: 0 14px;
    border: 0;
    border-radius: 18px;
    background: #3a3a3c;
    color: #e6fb04;
    font-family: Inter, sans-serif;
    font-size: 14px;
    font-weight: 600;
    cursor: pointer;
  }
  nav {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 8px 0;
  }
  .back {
    min-height: 44px;
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 0 8px;
    color: #e6fb04;
    font-size: 16px;
    font-weight: 500;
  }
  .back:hover {
    color: #e6fb04;
  }
  .more {
    width: 44px;
    height: 44px;
    border: 0;
    background: transparent;
    display: flex;
    align-items: center;
    justify-content: center;
    cursor: pointer;
  }
  .waiting {
    font-size: 14px;
    color: #8e8e93;
    padding-right: 12px;
  }
  .backdrop {
    position: absolute;
    inset: 0;
    z-index: 1;
    border: 0;
    background: transparent;
  }
  .menu {
    position: absolute;
    top: 52px;
    right: 8px;
    z-index: 2;
    width: min(320px, calc(100% - 16px));
    box-sizing: border-box;
    background: #2c2c2e;
    border-radius: 14px;
    padding: 14px;
    display: flex;
    flex-direction: column;
    gap: 10px;
    box-shadow: 0 10px 40px rgba(0, 0, 0, 0.7);
  }
  .scroll {
    flex: 1;
    overflow-y: auto;
    padding: 6px 16px 20px;
    display: flex;
    flex-direction: column;
    gap: 22px;
    scrollbar-width: none;
  }
  /* Cards keep their height; the column scrolls instead of squeezing them. */
  .scroll > * {
    flex-shrink: 0;
  }
  .scroll::-webkit-scrollbar {
    display: none;
  }
  .scroll.answer {
    padding: 6px 16px 16px;
    gap: 18px;
  }
  .gone {
    margin: 0;
    padding: 0 4px;
    font-size: 16px;
    color: #a1a1a6;
  }
  header {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 0 4px;
  }
  h1 {
    margin: 0;
    font-size: 26px;
    font-weight: 700;
    letter-spacing: -0.015em;
    line-height: 1.2;
    overflow-wrap: anywhere;
  }
  .status {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px 8px;
    font-size: 15px;
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: #8e8e93;
    flex: 0 0 auto;
  }
  .dot.blocked {
    background: #ff9f0a;
  }
  .dot.done {
    background: #64d2ff;
  }
  .dot.again {
    background: #e6fb04;
  }
  .state {
    font-weight: 600;
    white-space: nowrap;
  }
  .state.blocked {
    color: #ffb340;
  }
  .state.done {
    color: #7fd9ff;
  }
  .state.idle {
    color: #a1a1a6;
  }
  .where {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    font-size: 14px;
    color: #a1a1a6;
  }
  .where .ws {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .nav-actions {
    display: flex;
    align-items: center;
    gap: 2px;
  }
  .info {
    margin: 0 0 12px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .info div {
    display: flex;
    justify-content: space-between;
    gap: 12px;
    font-size: 15px;
  }
  .info dt {
    color: #a1a1a6;
  }
  .info dd {
    margin: 0;
    min-width: 0;
    overflow-wrap: anywhere;
    text-align: right;
  }
  .more-steps {
    min-height: 44px;
    border: 0;
    background: transparent;
    color: #e6fb04;
    font-family: Inter, sans-serif;
    font-size: 15px;
    font-weight: 500;
    align-self: flex-start;
    padding: 0 4px;
    cursor: pointer;
  }
  .card {
    background: #1c1c1e;
    border-radius: 14px;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
  }
  .label {
    font-size: 13px;
    font-weight: 600;
    color: #8e8e93;
  }
  .card p {
    margin: 0;
    overflow-wrap: anywhere;
    font-size: 17px;
    line-height: 1.45;
  }
  .card.top {
    gap: 12px;
  }
  .question {
    font-weight: 600;
  }
  .replies {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }
  .reply {
    min-height: 44px;
    padding: 0 16px;
    border-radius: 22px;
    border: 0;
    background: #2c2c2e;
    color: #f5f5f7;
    font-family: Inter, sans-serif;
    font-size: 15px;
    font-weight: 600;
    cursor: pointer;
  }
  .reply.primary {
    background: #e6fb04;
    color: #000;
  }
  .reply:disabled {
    cursor: default;
    opacity: 0.6;
  }
  .next-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .rows {
    background: #1c1c1e;
    border-radius: 14px;
    overflow: hidden;
  }
  .action {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 12px;
    min-height: 56px;
    padding: 8px 14px;
    box-sizing: border-box;
    border: 0;
    border-top: 0.5px solid #38383a;
    background: transparent;
    color: #f5f5f7;
    font-family: Inter, sans-serif;
    text-align: left;
    cursor: pointer;
  }
  .action:first-child {
    border-top: 0;
  }
  .action.info-row {
    cursor: default;
  }
  .glyph {
    width: 22px;
    display: flex;
    justify-content: center;
    flex: 0 0 auto;
  }
  .words {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .action-label {
    font-size: 16px;
    overflow-wrap: anywhere;
  }
  .action-sub {
    font-size: 13px;
    color: #8e8e93;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .chev {
    flex: 0 0 auto;
  }
  .term-hint {
    margin: 0;
    padding: 0 4px;
    font-size: 14px;
    color: #8e8e93;
  }
  /* Pulled up to the steps it follows; after the top card it keeps the page's gap. */
  .steps + .term-hint {
    margin-top: -14px;
  }
  .card.led {
    padding: 14px 16px;
  }
  .card.led p {
    font-size: 16px;
    color: #d1d1d6;
  }
  .steps {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  h2 {
    margin: 0 4px;
    font-size: 15px;
    font-weight: 600;
  }
  ol {
    list-style: none;
    margin: 0;
    padding: 0;
    background: #1c1c1e;
    border-radius: 14px;
    overflow: hidden;
  }
  li {
    display: flex;
    gap: 12px;
    padding: 12px 16px;
    border-top: 0.5px solid #38383a;
    align-items: flex-start;
  }
  li:first-child {
    border-top: 0;
  }
  .icon {
    width: 22px;
    display: flex;
    justify-content: center;
    padding-top: 1px;
    flex: 0 0 auto;
  }
  .text {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .what {
    font-size: 15px;
    line-height: 1.4;
    color: #d1d1d6;
  }
  .what.you,
  .what.now {
    color: #f5f5f7;
  }
  .meta {
    font-size: 13px;
    color: #8e8e93;
  }
  .lined {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .lined h2 {
    display: flex;
    align-items: baseline;
    gap: 8px;
  }
  .sub {
    font-size: 13px;
    font-weight: 500;
    color: #8e8e93;
  }
  .lined li {
    padding: 12px 8px 12px 16px;
    align-items: center;
  }
  .item {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 3px;
  }
  /* A long message shows its first three lines; the agent gets all of it. */
  .item-text {
    font-size: 15px;
    line-height: 1.4;
    color: #f5f5f7;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    display: -webkit-box;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 3;
    line-clamp: 3;
    overflow: hidden;
  }
  .item-text.finished {
    color: #a1a1a6;
  }
  .item-meta {
    font-size: 13px;
    color: #8e8e93;
    overflow-wrap: anywhere;
  }
  .item-meta.next {
    color: #e6fb04;
  }
  .remove {
    width: 44px;
    height: 44px;
    border: 0;
    background: transparent;
    display: flex;
    align-items: center;
    justify-content: center;
    cursor: pointer;
    flex: 0 0 auto;
  }
  .hint {
    font-size: 14px;
    color: #8e8e93;
  }
  .show-term {
    min-height: 48px;
    border: 0;
    border-radius: 12px;
    background: #3a3a3c;
    color: #f5f5f7;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 0 14px;
    font-family: Inter, sans-serif;
    font-size: 16px;
    cursor: pointer;
  }
  .term {
    position: absolute;
    inset: 0;
    z-index: 5;
    background: #000;
    display: flex;
    flex-direction: column;
    padding: 8px 12px calc(12px + env(safe-area-inset-bottom, 0px));
    box-sizing: border-box;
    gap: 8px;
  }
  .term-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding-left: 4px;
  }
  .term-title {
    display: flex;
    align-items: baseline;
    gap: 8px;
    min-width: 0;
    font-size: 17px;
    font-weight: 600;
  }
  .term-title .hint {
    font-weight: 400;
  }
  .done-btn {
    min-height: 44px;
    padding: 0 8px;
    border: 0;
    background: transparent;
    color: #e6fb04;
    font-family: Inter, sans-serif;
    font-size: 16px;
    font-weight: 600;
    cursor: pointer;
    flex: 0 0 auto;
  }
  .term pre {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
  }
  pre {
    margin: 0;
    background: #0d0d0d;
    border: 0.5px solid #2c2c2e;
    border-radius: 12px;
    padding: 12px;
    font-family: "JetBrains Mono", monospace;
    font-size: 11.5px;
    line-height: 1.55;
    color: #c7c7cc;
    white-space: pre-wrap;
    word-break: break-word;
  }
  footer {
    /* The safe-area inset is 0 off-phone, so this is the mockup's 22px there. */
    padding: 10px 12px calc(22px + env(safe-area-inset-bottom, 0px));
    display: flex;
    flex-direction: column;
    gap: 8px;
    background: rgba(0, 0, 0, 0.92);
    border-top: 0.5px solid #2c2c2e;
  }
  .view-only p {
    margin: 0;
    min-height: 44px;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 15px;
    color: #8e8e93;
  }
  .error {
    margin: 0 4px;
    font-size: 14px;
    color: #ffb340;
  }
  .bar-row {
    display: flex;
    gap: 8px;
    align-items: flex-end;
  }
  .stop {
    min-height: 36px;
    margin: 4px 0;
    padding: 0 14px;
    border-radius: 22px;
    border: 0;
    background: #2c2c2e;
    color: #f5f5f7;
    font-family: Inter, sans-serif;
    font-size: 15px;
    font-weight: 600;
    display: flex;
    align-items: center;
    gap: 6px;
    cursor: pointer;
    flex: 0 0 auto;
  }
  .compose {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin: 0;
  }
  .sr {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
  }
  .field {
    display: flex;
    align-items: flex-end;
    gap: 6px;
    min-height: 44px;
    border-radius: 22px;
    background: #1c1c1e;
    padding: 0 6px 0 16px;
  }
  textarea {
    flex: 1;
    min-width: 0;
    border: 0;
    outline: 0;
    background: transparent;
    color: #f5f5f7;
    font-family: Inter, sans-serif;
    font-size: 16px;
    line-height: 22px;
    padding: 11px 0;
    margin: 0;
    resize: none;
    /* Grows with its text, up to five lines, then scrolls. */
    field-sizing: content;
    min-height: 22px;
    max-height: 110px;
  }
  .echo {
    margin: 0;
    align-self: flex-end;
    max-width: 85%;
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 2px;
  }
  /* The padding sits outside the clamped text, so no part-line shows below it. */
  .echo-bubble {
    padding: 8px 12px;
    border-radius: 16px;
    background: #2c2c2e;
  }
  .echo-text {
    font-size: 15px;
    line-height: 20px;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    display: -webkit-box;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 3;
    line-clamp: 3;
    overflow: hidden;
  }
  .echo-state {
    font-size: 12px;
    color: #8e8e93;
    padding-right: 4px;
  }
  .echo-state.follow {
    font-size: 13px;
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
    justify-content: flex-end;
  }
  .now {
    min-height: 36px;
    padding: 0 14px;
    border: 0;
    border-radius: 18px;
    background: #3a3a3c;
    color: #e6fb04;
    font-family: Inter, sans-serif;
    font-size: 14px;
    font-weight: 600;
    cursor: pointer;
  }
  .send {
    flex: 0 0 auto;
    margin-bottom: 5px;
    width: 34px;
    height: 34px;
    border-radius: 50%;
    border: 0;
    background: #e6fb04;
    display: flex;
    align-items: center;
    justify-content: center;
    cursor: pointer;
  }
  .spin {
    animation: spin 1.1s linear infinite;
    transform-origin: 50% 50%;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .spin {
      animation: none;
    }
  }
</style>
