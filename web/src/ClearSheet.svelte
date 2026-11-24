<script lang="ts">
  // "Start fresh with /clear" asks first: a bottom sheet in the answer sheet's idiom over the agent view.
  import { clearBody } from "./lib/words";

  let { name, onconfirm, oncancel }: { name: string; onconfirm: () => void; oncancel: () => void } = $props();

  function onKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") oncancel();
  }
</script>

<svelte:window onkeydown={onKeydown} />

<button type="button" class="backdrop" aria-label="Cancel" onclick={oncancel}></button>
<div class="sheet" role="dialog" aria-modal="true" aria-labelledby="clear-title">
  <span class="handle" aria-hidden="true"></span>
  <div class="ask">
    <h2 id="clear-title">Start fresh?</h2>
    <p>{clearBody(name)}</p>
  </div>
  <div class="options">
    <button type="button" class="option destructive" onclick={onconfirm}>Send /clear</button>
    <button type="button" class="option" onclick={oncancel} {@attach (node) => node.focus()}>Cancel</button>
  </div>
</div>

<style>
  .backdrop {
    position: absolute;
    inset: 0;
    z-index: 3;
    border: 0;
    background: rgba(0, 0, 0, 0.55);
  }
  .sheet {
    position: absolute;
    left: 0;
    right: 0;
    bottom: 0;
    z-index: 4;
    background: #1c1c1e;
    border-radius: 24px 24px 0 0;
    padding: 10px 16px calc(24px + env(safe-area-inset-bottom, 0px));
    display: flex;
    flex-direction: column;
    gap: 16px;
    box-shadow: 0 -10px 40px rgba(0, 0, 0, 0.7);
    box-sizing: border-box;
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
  .ask {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  h2 {
    margin: 0;
    font-size: 20px;
    font-weight: 700;
  }
  p {
    margin: 0;
    font-size: 15px;
    line-height: 1.4;
    color: #a1a1a6;
    overflow-wrap: anywhere;
  }
  .options {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .option {
    min-height: 54px;
    border-radius: 14px;
    border: 0;
    background: #2c2c2e;
    color: #f5f5f7;
    font-family: Inter, sans-serif;
    font-size: 16px;
    font-weight: 600;
    cursor: pointer;
  }
  .option.destructive {
    background: #ff453a;
    color: #fff;
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
