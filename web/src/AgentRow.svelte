<script lang="ts">
  import { ageLabel, kindOf } from "./lib/agents";
  import type { Agent } from "./lib/protocol";
  import { agentHash } from "./lib/route";
  import { linedUpLine } from "./lib/words";

  /** lined: how many messages wait to go to this agent. */
  let { agent, now, lined }: { agent: Agent; now: number; lined: number } = $props();

  const kind = $derived(kindOf(agent.status));
</script>

<a href={agentHash(agent.pane_id)} class="row">
  <span class={["icon", kind]}>
    {#if kind === "ask"}
      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M6 6a2 2 0 1 1 2.8 1.8c-.5.3-.8.7-.8 1.3V10" stroke="#ff9f0a" stroke-width="1.8" fill="none" stroke-linecap="round"></path><circle cx="8" cy="12.6" r="1" fill="#ff9f0a"></circle></svg>
    {:else if kind === "done"}
      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" stroke="#64d2ff" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
    {:else if kind === "working"}
      <svg class="spin" width="18" height="18" viewBox="0 0 18 18" aria-hidden="true"><circle cx="9" cy="9" r="7" stroke="#3a3a3c" stroke-width="2" fill="none"></circle><path d="M9 2a7 7 0 0 1 7 7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
    {:else}
      <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><rect x="3" y="3" width="3" height="8" rx="1" fill="#8e8e93"></rect><rect x="8" y="3" width="3" height="8" rx="1" fill="#8e8e93"></rect></svg>
    {/if}
  </span>
  <span class="text">
    <span class="name">{agent.name}</span>
    <span class={["line", kind]}>{agent.line}</span>
    {#if lined > 0}
      <span class="lined">
        <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true"><path d="M2 3h8M2 6h8M2 9h5" stroke="#8e8e93" stroke-width="1.4" fill="none" stroke-linecap="round"></path></svg>
        {linedUpLine(lined)}
      </span>
    {/if}
  </span>
  <span class="meta">
    <span class="age">{ageLabel(agent, now)}</span>
    <span class="ws">{agent.workspace_label}</span>
  </span>
  <svg class="chevron" width="8" height="14" viewBox="0 0 8 14" aria-hidden="true"><path d="M1.5 1.5L6.5 7l-5 5.5" stroke="#48484a" stroke-width="1.8" fill="none" stroke-linecap="round"></path></svg>
</a>

<style>
  .row {
    display: flex;
    align-items: center;
    gap: 12px;
    min-height: 68px;
    padding: 10px 14px;
    box-sizing: border-box;
    border-top: 0.5px solid #38383a;
  }
  .row:first-child {
    border-top: 0;
  }
  .icon {
    width: 32px;
    height: 32px;
    border-radius: 50%;
    background: #2c2c2e;
    display: flex;
    align-items: center;
    justify-content: center;
    flex: 0 0 auto;
  }
  .icon.ask {
    background: rgba(255, 159, 10, 0.16);
  }
  .icon.done {
    background: rgba(100, 210, 255, 0.14);
  }
  .text {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 3px;
  }
  .name {
    font-size: 16px;
    font-weight: 600;
    line-height: 1.25;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .line {
    font-size: 14px;
    line-height: 1.3;
    color: #a1a1a6;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .line.ask {
    color: #ffb340;
  }
  .line.done {
    color: #7fd9ff;
  }
  .lined {
    font-size: 13px;
    color: #8e8e93;
    display: flex;
    align-items: center;
    gap: 5px;
  }
  .lined svg {
    flex: 0 0 auto;
  }
  .meta {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 4px;
    flex: 0 0 auto;
  }
  .age {
    font-size: 13px;
    color: #8e8e93;
  }
  .ws {
    font-size: 12px;
    color: #8e8e93;
  }
  .chevron {
    flex: 0 0 auto;
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
