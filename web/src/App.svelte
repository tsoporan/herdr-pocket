<script lang="ts">
  import { type Agents, applyPatch, fromSnapshot, sections } from "./lib/agents";
  import type { Detail, Expiry, QueueItem, Role, ServerMessage, StartJob, StartKind, Workspace } from "./lib/protocol";
  import { staleNotifications } from "./lib/push";
  import { agentItems, waitingCount } from "./lib/queue";
  import { NEW_HASH, SETTINGS_HASH, parseRoute } from "./lib/route";
  import { workspaceList } from "./lib/start";
  import { connect, type Link } from "./lib/socket";
  import { expiryBody, expiryTitle, linkWords } from "./lib/words";
  import AgentRow from "./AgentRow.svelte";
  import AgentView from "./AgentView.svelte";
  import ExpirySheet from "./ExpirySheet.svelte";
  import NewTask from "./NewTask.svelte";
  import Settings from "./Settings.svelte";

  let agents: Agents = $state({});
  // Work lined up for agents, replaced whole by the snapshot and by any patch that carries it.
  let lined = $state<QueueItem[]>([]);
  // herdr's workspaces, and what New task may start: the kinds, and every login's start jobs.
  let workspaces = $state<Record<string, Workspace>>({});
  let startKinds = $state<StartKind[]>([]);
  let starts = $state<StartJob[]>([]);
  // The workspace of the agent last opened: New task's default "Where".
  let lastWorkspace = $state<string | null>(null);
  let host = $state("");
  // Until the first snapshot says otherwise, nothing that sends input is shown.
  let role = $state<Role>("reader");
  let expiry = $state<Expiry | null>(null);
  let vapidKey = $state<string | null>(null);
  // The server's bundle when this page loaded (not reactive: only compared).
  let build: string | undefined;
  // True once a snapshot has arrived, so the agents are the server's and not the empty start.
  let loaded = $state(false);
  let fixOpen = $state(false);
  // A snapshot has arrived on the current socket (watches go out only then).
  let connected = $state(false);
  // The link as the socket reports it, and when the last frame came.
  let link = $state<Link>({ state: "connecting", retryAt: null });
  let lastUpdate = $state<number | null>(null);
  let now = $state(Date.now());
  let route = $state(parseRoute(location.hash));
  // Only the open agent's detail and screen are kept; they stay while disconnected.
  let detail = $state<Detail | null>(null);
  let output = $state<{ pane_id: string; text: string } | null>(null);

  const list = $derived(sections(agents));
  const count = $derived(Object.keys(agents).length);
  const paneId = $derived(route.view === "agent" ? route.paneId : null);
  const workspaceRows = $derived(workspaceList(workspaces));
  $effect(() => {
    const ws = paneId ? agents[paneId]?.workspace_id : undefined;
    if (ws) lastWorkspace = ws;
  });

  function onMessage(msg: ServerMessage) {
    now = Date.now();
    lastUpdate = now;
    switch (msg.type) {
      case "snapshot":
        // The server was updated while this page stayed open (an installed
        // app lives on in the background): load the new bundle.
        if (build && msg.build && msg.build !== build) {
          location.reload();
          return;
        }
        build = msg.build ?? build;
        agents = fromSnapshot(msg);
        lined = msg.queue ?? [];
        workspaces = Object.fromEntries((msg.workspaces ?? []).map((w) => [w.workspace_id, w]));
        startKinds = msg.start_kinds ?? [];
        starts = msg.starts ?? [];
        host = msg.host;
        role = msg.role;
        expiry = msg.expiry;
        vapidKey = msg.push?.vapid_public_key || null;
        loaded = true;
        connected = true;
        break;
      case "patch":
        agents = applyPatch(agents, msg);
        if (msg.queue !== undefined) lined = msg.queue ?? [];
        if (msg.starts !== undefined) starts = msg.starts ?? [];
        if (msg.workspaces) {
          const next = { ...workspaces };
          for (const [id, w] of Object.entries(msg.workspaces)) {
            if (w === null) delete next[id];
            else next[id] = w;
          }
          workspaces = next;
        }
        // An absent field is unchanged; a null expiry clears the banner.
        if (msg.role !== undefined) role = msg.role;
        if (msg.expiry !== undefined) expiry = msg.expiry;
        break;
      // Frames for a pane just left can arrive after its unwatch.
      case "detail":
        // Tolerate a server that omits or nulls the optional parts.
        if (msg.pane_id === paneId)
          detail = { ...msg, steps: msg.steps ?? [], context_pct: msg.context_pct ?? null, pr: msg.pr ?? null };
        break;
      case "output":
        if (msg.pane_id === paneId) output = msg;
        break;
    }
  }

  // The last list stays on screen while disconnected.
  const socket = connect({
    onMessage,
    onDown: () => (connected = false),
    onLink: (l) => (link = l),
  });
  $effect(() => () => socket.close());

  // Back in view, or back online: reconnect now (a fresh socket after a long time away,
  // since the phone may have killed the old one silently), not after a backoff.
  $effect(() => {
    let hiddenAt: number | null = document.visibilityState === "hidden" ? Date.now() : null;
    const onVisibility = () => {
      if (document.visibilityState === "hidden") {
        hiddenAt = Date.now();
        return;
      }
      const away = hiddenAt === null ? 0 : Date.now() - hiddenAt;
      hiddenAt = null;
      socket.resume(away);
    };
    const onOnline = () => socket.resume();
    const onOffline = () => socket.offline();
    document.addEventListener("visibilitychange", onVisibility);
    addEventListener("online", onOnline);
    addEventListener("offline", onOffline);
    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      removeEventListener("online", onOnline);
      removeEventListener("offline", onOffline);
    };
  });

  $effect(() => {
    const onHash = () => (route = parseRoute(location.hash));
    addEventListener("hashchange", onHash);
    return () => removeEventListener("hashchange", onHash);
  });

  // Watch the open agent; re-runs on every (re)connect, since the server forgets watches with the socket.
  $effect(() => {
    if (!paneId || !connected) return;
    const id = paneId;
    socket.watch({ type: "watch", pane_id: id });
    return () => socket.watch({ type: "unwatch", pane_id: id });
  });

  // Close the alerts that no longer hold, e.g. a blocked agent answered on the desktop.
  // Runs on open (the first snapshot) and whenever the agents change.
  $effect(() => {
    if (!loaded || !("serviceWorker" in navigator)) return;
    const current = agents;
    navigator.serviceWorker
      .getRegistration("/")
      .then(async (reg) => {
        if (!reg) return;
        for (const n of staleNotifications(await reg.getNotifications(), current)) n.close();
      })
      .catch((err) => console.error("herdr-pocket: notifications", err));
  });

  // The clock for ages; every second while the link isn't live, so "last update"
  // and "trying again in" count.
  const live = $derived(link.state === "live");
  $effect(() => {
    const tick = setInterval(() => (now = Date.now()), live ? 15000 : 1000);
    return () => clearInterval(tick);
  });
</script>

{#if paneId}
  {#key paneId}
    <AgentView
      {paneId}
      {agents}
      detail={detail?.pane_id === paneId ? detail : null}
      output={output?.pane_id === paneId ? output.text : null}
      {connected}
      {link}
      {lastUpdate}
      onretry={() => socket.retry()}
      {loaded}
      {now}
      {socket}
      lined={agentItems(lined, paneId)}
      reader={role === "reader"}
    />
  {/key}
{:else if route.view === "settings"}
  <Settings {socket} {connected} {vapidKey} />
{:else if route.view === "new"}
  <NewTask
    {socket}
    {loaded}
    reader={role === "reader"}
    {agents}
    workspaces={workspaceRows}
    kinds={startKinds}
    {starts}
    {lastWorkspace}
  />
{:else}
  <main>
    <header>
      <div class="actions">
        <a href={SETTINGS_HASH} class="settings">Settings</a>
        <!-- Select (several agents) comes with that feature; until then the + stands alone. -->
        {#if loaded && role === "controller" && startKinds.length}
          <a href={NEW_HASH} class="new" aria-label="New agent">
            <svg width="20" height="20" viewBox="0 0 20 20" aria-hidden="true"><path d="M10 3.5v13M3.5 10h13" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
          </a>
        {/if}
      </div>
      <h1>Agents</h1>
      <div class="status">
        <span class={["dot", link.state]}></span>
        <span class="status-words">
          {#if link.state === "live"}
            Connected to {host} · {count} {count === 1 ? "agent" : "agents"}
          {:else}
            {@const w = linkWords(link.state, link.retryAt, lastUpdate, now)}
            <span>{w.title}</span>
            {#if w.detail}<span class="status-detail">{w.detail}</span>{/if}
          {/if}
        </span>
        {#if link.state === "down" || link.state === "offline"}
          <button type="button" class="retry" onclick={() => socket.retry()}>Retry</button>
        {/if}
      </div>
    </header>

    {#if expiry}
      <div role="status" class="expiry">
        <svg class="clock" width="20" height="20" viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="8.5" stroke="#ff9f0a" stroke-width="1.6" fill="none"></circle><path d="M10 5.5v5l3 2" stroke="#ff9f0a" stroke-width="1.6" fill="none" stroke-linecap="round"></path></svg>
        <div class="expiry-text">
          <span class="expiry-title">{expiryTitle(expiry.days_left)}</span>
          <span class="expiry-body">{expiryBody(expiry.days_left, host)}</span>
          <button type="button" class="fix" onclick={() => (fixOpen = true)}>How to fix</button>
        </div>
      </div>
    {/if}

    {#each list as section (section.key)}
      <section aria-label={section.title}>
        <h2 class={section.key}>{section.title}<span class="count">{section.rows.length}</span></h2>
        <div class="group">
          {#each section.rows as agent (agent.pane_id)}
            <AgentRow {agent} {now} lined={waitingCount(lined, agent.pane_id)} />
          {/each}
        </div>
      </section>
    {/each}
  </main>
  {#if fixOpen}
    <ExpirySheet ondone={() => (fixOpen = false)} />
  {/if}
{/if}

<style>
  main {
    min-height: 100dvh;
    box-sizing: border-box;
    padding: 0 16px 32px;
  }
  /* The top row and the title sit where Settings and the agent view put
     their back link and title (8px and 58px down), so nothing jumps. */
  header {
    padding: 8px 4px 14px;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .actions {
    display: flex;
    justify-content: space-between;
  }
  /* The same type, size and colour as the back links on the other screens. */
  .settings {
    min-height: 44px;
    display: flex;
    align-items: center;
    padding: 0 4px;
    margin: 0;
    border: 0;
    background: none;
    font: inherit;
    font-size: 16px;
    font-weight: 500;
    color: #e6fb04;
    cursor: pointer;
  }
  .settings:hover {
    color: #e6fb04;
  }
  .new {
    width: 44px;
    height: 44px;
    display: flex;
    align-items: center;
    justify-content: center;
  }
  h1 {
    margin: 2px 0 0;
    font-size: 34px;
    font-weight: 700;
    letter-spacing: -0.02em;
  }
  .status {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 14px;
    color: #a1a1a6;
  }
  .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: #8e8e93;
  }
  .dot.live {
    background: #30d158;
  }
  .dot.slow,
  .dot.connecting {
    background: #ff9f0a;
  }
  .dot.down,
  .dot.offline {
    background: #ff453a;
  }
  .status-words {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .status-detail {
    font-size: 13px;
    color: #8e8e93;
  }
  .retry {
    margin-left: auto;
    min-height: 36px;
    padding: 0 14px;
    border: 0;
    border-radius: 18px;
    background: #2c2c2e;
    color: #e6fb04;
    font-family: Inter, sans-serif;
    font-size: 14px;
    font-weight: 600;
    cursor: pointer;
  }
  .expiry {
    margin: 4px 0 18px;
    background: #1c1c1e;
    border-radius: 14px;
    padding: 14px;
    display: flex;
    gap: 12px;
    align-items: flex-start;
  }
  .clock {
    flex: 0 0 auto;
    margin-top: 1px;
  }
  .expiry-text {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .expiry-title {
    font-size: 15px;
    font-weight: 600;
  }
  .expiry-body {
    font-size: 14px;
    line-height: 1.4;
    color: #a1a1a6;
  }
  /* The mockup's "How to fix" is text; as a button it keeps the same type.
     It is 44px tall to tap (SPEC UX 6); the negative margins keep the text
     where the mockup has it (19px line, 4px above it). */
  .fix {
    align-self: flex-start;
    min-height: 44px;
    display: flex;
    align-items: center;
    margin: -8.5px 0 -12.5px;
    padding: 0;
    border: 0;
    background: none;
    font-family: inherit;
    font-size: 15px;
    font-weight: 500;
    color: #e6fb04;
    cursor: pointer;
  }
  section {
    margin-bottom: 26px;
  }
  h2 {
    margin: 0 4px 8px;
    font-size: 15px;
    font-weight: 600;
    color: #f5f5f7;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  h2.needs {
    color: #ff9f0a;
  }
  .count {
    font-size: 13px;
    font-weight: 500;
    color: #8e8e93;
  }
  .group {
    background: #1c1c1e;
    border-radius: 14px;
    overflow: hidden;
  }
</style>
