<script lang="ts">
  import { QUICK_KEYS } from "./lib/words";

  let { onkey }: { onkey: (key: string) => void } = $props();
</script>

<div class="keys">
  {#each QUICK_KEYS as k (k.key)}
    <button type="button" aria-label={k.name} onclick={() => onkey(k.key)}>{k.label}</button>
  {/each}
</div>

<style>
  .keys {
    display: grid;
    grid-template-columns: repeat(5, minmax(0, 1fr));
    gap: 8px;
  }
  button {
    min-height: 44px;
    border-radius: 12px;
    border: 0;
    background: #3a3a3c;
    color: #f5f5f7;
    font-family: Inter, sans-serif;
    font-size: 15px;
    font-weight: 600;
    cursor: pointer;
    padding: 0;
  }
</style>
