<script lang="ts">
  // The agent's last message in the top card, at reading size. Transcript text
  // is Markdown, parsed by lib/message.ts and rendered as text (never HTML); a terminal agent's screen lines
  // stay as they are. Past about 12 lines it clamps, one tap from the rest.
  import { formatMessage, type Span } from "./lib/message";

  // clamp: false for short text that must always show whole, such as the question above the reply buttons.
  let { text, plain, clamp = true }: { text: string; plain: boolean; clamp?: boolean } = $props();

  const CLAMP_LINES = 12;
  const blocks = $derived(plain ? [] : formatMessage(text));
  // Expanding is for this message; a new one starts clamped.
  let expandedFor = $state<string | null>(null);
  const expanded = $derived(expandedFor === text);
  let long = $state(false);

  // Whether the whole message is taller than the clamp, re-measured as it changes or reflows.
  function measure(node: HTMLElement) {
    const check = () => {
      const lineHeight = parseFloat(getComputedStyle(node).lineHeight);
      long = clamp && node.scrollHeight > lineHeight * CLAMP_LINES + 1;
    };
    const ro = new ResizeObserver(check);
    ro.observe(node);
    check();
    return () => ro.disconnect();
  }
</script>

{#snippet inline(line: Span[])}{#each line as s, j (j)}{#if s.code}<code>{s.text}</code>{:else if s.bold || s.italic || s.strike || s.link}<span class={[s.bold && "b", s.italic && "i", s.strike && "s", s.link && "a"]}>{s.text}</span>{:else}{s.text}{/if}{/each}{/snippet}

<div class={["clip", long && !expanded && "clamped"]}>
  <div class={["body", plain && "plain"]} {@attach measure}>
    {#if plain}
      {text}
    {:else}
      {#each blocks as block, b (b)}
        {#if block.kind === "p"}
          <p>
            {#each block.lines as line, i (i)}{#if i > 0}<br />{/if}{@render inline(line)}{/each}
          </p>
        {:else if block.kind === "h"}
          <p class={["h", block.level <= 2 && "h-big"]} role="heading" aria-level={Math.min(block.level + 2, 6)}>{@render inline(block.spans)}</p>
        {:else if block.kind === "list"}
          <ul>
            {#each block.items as item, i (i)}
              <li class={`d${item.depth}`}><span class="mark" aria-hidden="true">{item.marker}</span><span>{@render inline(item.spans)}</span></li>
            {/each}
          </ul>
        {:else if block.kind === "quote"}
          <blockquote>
            {#each block.lines as line, i (i)}{#if i > 0}<br />{/if}{@render inline(line)}{/each}
          </blockquote>
        {:else if block.kind === "pre"}
          <pre><code>{block.text}</code></pre>
        {:else if block.kind === "table"}
          <div class="table">
            <table>
              <thead><tr>{#each block.head as cell, i (i)}<th>{@render inline(cell)}</th>{/each}</tr></thead>
              <tbody>
                {#each block.rows as row, r (r)}
                  <tr>{#each row as cell, i (i)}<td>{@render inline(cell)}</td>{/each}</tr>
                {/each}
              </tbody>
            </table>
          </div>
        {:else}
          <hr />
        {/if}
      {/each}
    {/if}
  </div>
</div>
{#if long}
  <button type="button" class="whole" aria-expanded={expanded} onclick={() => (expandedFor = expanded ? null : text)}>
    {expanded ? "Show less" : "Show the whole message"}
  </button>
{/if}

<style>
  .clip.clamped {
    /* 12 lines of the body's 17px × 1.45. */
    max-height: calc(12 * 1.45 * 17px);
    overflow: hidden;
    mask-image: linear-gradient(to bottom, #000 75%, transparent);
  }
  .body {
    font-size: 17px;
    line-height: 1.45;
    overflow-wrap: anywhere;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .body.plain {
    display: block;
    white-space: pre-wrap;
  }
  p,
  ul {
    margin: 0;
  }
  ul {
    list-style: none;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  li {
    display: flex;
    gap: 8px;
  }
  li.d1 {
    padding-left: 20px;
  }
  li.d2 {
    padding-left: 40px;
  }
  li.d3 {
    padding-left: 60px;
  }
  .mark {
    color: #8e8e93;
    flex: none;
    min-width: 12px;
    font-variant-numeric: tabular-nums;
  }
  li > span:last-child {
    min-width: 0;
  }
  .h {
    font-weight: 650;
    margin-top: 4px;
  }
  .h-big {
    font-size: 19px;
  }
  .b {
    font-weight: 650;
  }
  .i {
    font-style: italic;
  }
  .s {
    text-decoration: line-through;
  }
  .a {
    text-decoration: underline;
    text-decoration-color: #636366;
    text-underline-offset: 3px;
  }
  blockquote {
    margin: 0;
    padding-left: 12px;
    border-left: 3px solid #48484a;
    color: #c7c7cc;
  }
  pre {
    margin: 0;
    padding: 10px 12px;
    background: #2c2c2e;
    border-radius: 8px;
    overflow-x: auto;
    overflow-wrap: normal;
  }
  pre code {
    background: none;
    padding: 0;
    font-size: 13px;
    line-height: 1.5;
    white-space: pre;
  }
  .table {
    overflow-x: auto;
  }
  table {
    border-collapse: collapse;
    font-size: 15px;
  }
  th,
  td {
    text-align: left;
    vertical-align: top;
    padding: 6px 10px 6px 0;
    border-bottom: 1px solid #38383a;
    overflow-wrap: normal;
    min-width: 72px;
  }
  th {
    font-weight: 650;
    color: #c7c7cc;
  }
  hr {
    border: 0;
    border-top: 1px solid #38383a;
    margin: 2px 0;
    width: 100%;
  }
  code {
    font-family: "JetBrains Mono", monospace;
    font-size: 15px;
    background: #2c2c2e;
    border-radius: 5px;
    padding: 1px 5px;
  }
  .whole {
    min-height: 44px;
    margin: -6px 0 -10px;
    border: 0;
    background: transparent;
    color: #e6fb04;
    font-family: Inter, sans-serif;
    font-size: 15px;
    font-weight: 500;
    align-self: flex-start;
    padding: 0;
    cursor: pointer;
  }
</style>
