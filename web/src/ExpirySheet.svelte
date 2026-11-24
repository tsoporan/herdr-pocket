<script lang="ts">
  // "How to fix" for the expiry banner, styled as the answer sheet
  // (AnswerSheet.svelte). It overlays the Agents list.
  let { ondone }: { ondone: () => void } = $props();

  function onKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") ondone();
  }
</script>

<svelte:window onkeydown={onKeydown} />

<button type="button" class="backdrop" aria-label="Close" onclick={ondone}></button>
<div class="sheet" role="dialog" aria-modal="true" aria-labelledby="expiry-title">
  <span class="handle" aria-hidden="true"></span>
  <h2 id="expiry-title">Keep herdr-pocket signed in</h2>
  <ol>
    <li>
      <span class="num" aria-hidden="true">1</span>
      <div class="words">
        <span class="title">Turn off key expiry</span>
        <span class="sub">In the Tailscale admin console, open Machines, find herdr-pocket, and choose Disable key expiry.</span>
        <a class="link" href="https://login.tailscale.com/admin/machines" target="_blank" rel="noopener">Open admin console</a>
      </div>
    </li>
    <li>
      <span class="num" aria-hidden="true">2</span>
      <div class="words">
        <span class="title">Or sign in again</span>
        <span class="sub">On the computer, run this and open the link it prints:</span>
        <code>herdr-pocket login</code>
      </div>
    </li>
  </ol>
  <button type="button" class="done" {@attach (node) => node.focus()} onclick={ondone}>Done</button>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 10;
    border: 0;
    background: rgba(0, 0, 0, 0.5);
  }
  .sheet {
    position: fixed;
    left: 0;
    right: 0;
    bottom: 0;
    z-index: 11;
    background: #1c1c1e;
    border-radius: 24px 24px 0 0;
    padding: 10px 16px calc(24px + env(safe-area-inset-bottom, 0px));
    display: flex;
    flex-direction: column;
    gap: 14px;
    box-shadow: 0 -10px 40px rgba(0, 0, 0, 0.7);
    max-height: 85dvh;
    box-sizing: border-box;
    overflow-y: auto;
    animation: slide 0.32s cubic-bezier(0.2, 0.8, 0.2, 1);
  }
  .handle {
    align-self: center;
    width: 36px;
    height: 5px;
    border-radius: 3px;
    background: #48484a;
    flex: 0 0 auto;
  }
  h2 {
    margin: 0;
    font-size: 20px;
    font-weight: 700;
  }
  ol {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }
  li {
    display: flex;
    gap: 12px;
    align-items: flex-start;
  }
  .num {
    width: 28px;
    height: 28px;
    border-radius: 50%;
    background: rgba(230, 251, 4, 0.14);
    color: #e6fb04;
    font-size: 15px;
    font-weight: 600;
    display: flex;
    align-items: center;
    justify-content: center;
    flex: 0 0 auto;
  }
  .words {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding-top: 3px;
  }
  .title {
    font-size: 16px;
    font-weight: 600;
  }
  .sub {
    font-size: 14px;
    line-height: 1.4;
    color: #a1a1a6;
  }
  code {
    display: block;
    margin-top: 4px;
    background: #0d0d0d;
    border-radius: 12px;
    padding: 12px 10px;
    font-family: "JetBrains Mono", monospace;
    font-size: 14px;
    line-height: 1.5;
    color: #f5f5f7;
    /* One command per line; a narrow phone scrolls rather than splitting
       a command at its hyphen. */
    white-space: pre;
    overflow-x: auto;
  }
  .link {
    align-self: flex-start;
    margin-top: 6px;
    min-height: 44px;
    display: flex;
    align-items: center;
    padding: 0 16px;
    border-radius: 14px;
    background: #2c2c2e;
    color: #f5f5f7;
    font-size: 15px;
    font-weight: 600;
  }
  .link:hover {
    color: #f5f5f7;
  }
  .done {
    min-height: 50px;
    border-radius: 14px;
    border: 0;
    background: #e6fb04;
    color: #000;
    font-family: Inter, sans-serif;
    font-size: 16px;
    font-weight: 600;
    cursor: pointer;
  }
  @keyframes slide {
    from {
      transform: translateY(100%);
    }
    to {
      transform: none;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .sheet {
      animation: none;
    }
  }
</style>
