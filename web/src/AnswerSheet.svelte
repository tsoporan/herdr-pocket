<script lang="ts">
  // The answer sheet, for permission prompts and Claude Code's questions.
  // AgentView keys it by the question id, so a new question starts with the
  // note closed and empty.
  import type { Question, QuestionOption } from "./lib/protocol";
  import QuickKeys from "./QuickKeys.svelte";

  let {
    question,
    reader,
    sent,
    sentSub,
    busy,
    error,
    onanswer,
    onkey,
    onstay,
    onnext,
    hasNext,
  }: {
    question: Question;
    /** Shows the question without anything that answers it. */
    reader: boolean;
    sent: string | null;
    /** The line under the sent title. */
    sentSub: string;
    busy: boolean;
    error: string | null;
    onanswer: (option: QuestionOption, note?: string) => void;
    onkey: (key: string) => void;
    onstay: () => void;
    onnext: () => void;
    /** Another agent in Needs you; without one, Next goes back to the list and says so. */
    hasNext: boolean;
  } = $props();

  // The option whose note field is open: "What it should do instead" for a
  // permission, the answer itself for a question's own-answer row.
  let denying: QuestionOption | null = $state(null);
  let note = $state("");

  // A multi-question ask's steps: its tabs, then the review. The current one
  // is the review on the review step, else the first unanswered tab.
  const steps = $derived(question.steps ?? []);
  const reviewing = $derived(!!question.review);
  const current = $derived(reviewing ? steps.length : steps.findIndex((s) => !s.done));
  const several = $derived(steps.length > 1);
  const foot = $derived(
    question.kind === "permission"
      ? "Read from the agent's screen. Your choice is sent as the matching option number."
      : reviewing
        ? "Read from the agent's review step."
        : several
          ? "Each answer goes to the agent as you tap it. You check them all before they're sent."
          : "Read from the agent's screen. A tap answers it straight away.",
  );

  function pick(option: QuestionOption) {
    if (option.needs_note) denying = option;
    else onanswer(option);
  }

  function sendInstead(e: SubmitEvent) {
    e.preventDefault();
    if (denying) onanswer(denying, note.trim() || undefined);
  }
</script>

{#if sent}
  <section class="done rise" aria-live="polite">
    <div class="head">
      <span class="badge">
        <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
      </span>
      <div class="words">
        <span class="title">{sent}</span>
        <span class="sub">{sentSub}</span>
      </div>
    </div>
    <div class="pair">
      <button type="button" class="stay" onclick={onstay}>Stay here</button>
      <button type="button" class="next" onclick={onnext}>{hasNext ? "Next waiting" : "Back to agents"}</button>
    </div>
  </section>
{:else}
  <section class="sheet" aria-label="Answer">
    <span class="handle" aria-hidden="true"></span>
    {#if several}
      <ol class="steps" aria-label="Questions">
        {#each steps as s, i (i)}
          <li class={[i === current && "on", s.done && "did"]} aria-current={i === current ? "step" : undefined}>{s.header}</li>
        {/each}
        <li class={[reviewing && "on"]} aria-current={reviewing ? "step" : undefined}>Review</li>
      </ol>
    {/if}
    <div class="ask">
      {#if several && !reviewing && current >= 0}
        <small>QUESTION {current + 1} OF {steps.length}</small>
      {/if}
      <h2>{question.title}</h2>
      {#if question.where}
        <span class="where">{question.command ? `It will run in ${question.where}:` : question.where}</span>
      {/if}
    </div>
    {#if question.command}
      <code>{question.command}</code>
    {/if}
    {#if question.review}
      <dl class="review">
        {#each question.review as r, i (i)}
          <div>
            <dt>{r.question}</dt>
            <dd>{r.answer}</dd>
          </div>
        {/each}
      </dl>
    {/if}
    {#if error}
      <p class="error" role="alert">{error}</p>
    {/if}
    {#if reader}
      {#if question.kind === "unknown" && question.screen}
        <pre>{question.screen}</pre>
      {/if}
      <span class="foot">You can view this agent but not answer it.</span>
    {:else if question.kind === "unknown"}
      {#if question.screen}
        <pre>{question.screen}</pre>
      {/if}
      <QuickKeys {onkey} />
      <span class="foot">Couldn't read the choices from the agent's screen. Keys go straight to the agent.</span>
    {:else}
      <div class="options">
        {#each question.options as o (o.key)}
          <button type="button" class={["option", o.primary && "primary", o.own && "own"]} disabled={busy} onclick={() => pick(o)}>
            <span class="label">{o.own ? `${o.label}…` : o.label}</span>
            {#if o.sub}
              <span class="sub">{o.sub}</span>
            {/if}
          </button>
        {/each}
      </div>
      {#if denying}
        <form class="instead rise" onsubmit={sendInstead}>
          <label for="instead" class="sr">{denying.own ? "Your own answer" : "What it should do instead"}</label>
          <input
            id="instead"
            type="text"
            placeholder={denying.own ? "Your own answer" : "What should it do instead?"}
            autocomplete="off"
            bind:value={note}
            {@attach (node) => node.focus()}
          />
          <button type="submit" disabled={busy || (denying.own && !note.trim())}>Send</button>
        </form>
      {/if}
      <span class="foot">{foot}</span>
    {/if}
  </section>
{/if}

<style>
  .sheet {
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
  /* A long sheet scrolls; its parts keep their height instead of squashing. */
  .sheet > :global(*) {
    flex-shrink: 0;
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
  .ask small {
    font-size: 13px;
    font-weight: 600;
    color: #8e8e93;
    letter-spacing: 0.02em;
  }
  .steps {
    display: flex;
    gap: 6px;
    margin: 0 -16px;
    padding: 0 16px;
    list-style: none;
    overflow-x: auto;
    scrollbar-width: none;
    flex: 0 0 auto;
  }
  .steps li {
    flex: 0 0 auto;
    max-width: 140px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 13px;
    font-weight: 600;
    padding: 5px 10px;
    border-radius: 99px;
    background: #2c2c2e;
    color: #a1a1a6;
  }
  .steps li.did {
    color: #f5f5f7;
  }
  .steps li.did::before {
    content: "✓ ";
    color: #e6fb04;
  }
  .steps li.on {
    background: #f5f5f7;
    color: #000;
  }
  .review {
    margin: 0;
    display: flex;
    flex-direction: column;
    background: #2c2c2e;
    border-radius: 14px;
    overflow: hidden;
  }
  .review div {
    padding: 10px 16px;
    display: flex;
    flex-direction: column;
    gap: 2px;
    border-top: 0.5px solid #3a3a3c;
  }
  .review div:first-child {
    border-top: 0;
  }
  .review dt {
    font-size: 13px;
    color: #8e8e93;
  }
  .review dd {
    margin: 0;
    font-size: 16px;
    font-weight: 600;
  }
  h2 {
    margin: 0;
    font-size: 20px;
    font-weight: 700;
  }
  .where {
    font-size: 15px;
    color: #a1a1a6;
  }
  code {
    display: block;
    background: #0d0d0d;
    border-radius: 12px;
    padding: 12px 14px;
    font-family: "JetBrains Mono", monospace;
    font-size: 14px;
    line-height: 1.5;
    color: #f5f5f7;
    white-space: pre-wrap;
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
  .error {
    margin: 0;
    font-size: 15px;
    color: #ffb340;
  }
  .options {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .option {
    min-height: 58px;
    border-radius: 14px;
    border: 0;
    background: #2c2c2e;
    color: #f5f5f7;
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    justify-content: center;
    gap: 2px;
    padding: 8px 16px;
    text-align: left;
    cursor: pointer;
    font-family: Inter, sans-serif;
  }
  .option.primary {
    background: #e6fb04;
    color: #000;
  }
  .option.own {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px #48484a;
  }
  .option.own .label {
    font-weight: 500;
  }
  .option:disabled {
    cursor: default;
  }
  .label {
    font-size: 16px;
    font-weight: 600;
  }
  .option .sub {
    font-size: 13px;
    color: #a1a1a6;
  }
  .option.primary .sub {
    color: #3d4300;
  }
  .instead {
    display: flex;
    gap: 8px;
    align-items: center;
    margin: 0;
  }
  .sr {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
  }
  .instead input {
    flex: 1;
    min-width: 0;
    min-height: 46px;
    box-sizing: border-box;
    border-radius: 23px;
    border: 0;
    background: #2c2c2e;
    color: #f5f5f7;
    padding: 0 16px;
    font-family: Inter, sans-serif;
    font-size: 16px;
  }
  .instead button {
    min-height: 46px;
    padding: 0 16px;
    border-radius: 23px;
    border: 0;
    background: #e6fb04;
    color: #000;
    font-family: Inter, sans-serif;
    font-size: 15px;
    font-weight: 600;
    cursor: pointer;
  }
  .foot {
    font-size: 12px;
    color: #8e8e93;
    text-align: center;
  }
  .done {
    background: #1c1c1e;
    border-radius: 24px 24px 0 0;
    padding: 20px 16px calc(26px + env(safe-area-inset-bottom, 0px));
    display: flex;
    flex-direction: column;
    gap: 16px;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 12px;
  }
  .badge {
    width: 36px;
    height: 36px;
    border-radius: 50%;
    background: rgba(230, 251, 4, 0.14);
    display: flex;
    align-items: center;
    justify-content: center;
    flex: 0 0 auto;
  }
  .words {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .title {
    font-size: 16px;
    font-weight: 600;
  }
  .words .sub {
    font-size: 14px;
    color: #a1a1a6;
  }
  .pair {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 8px;
  }
  .pair button {
    min-height: 50px;
    border-radius: 14px;
    border: 0;
    font-family: Inter, sans-serif;
    font-size: 16px;
    font-weight: 600;
    cursor: pointer;
  }
  .stay {
    background: #2c2c2e;
    color: #f5f5f7;
  }
  .next {
    background: #e6fb04;
    color: #000;
  }
  .rise {
    animation: rise 0.24s cubic-bezier(0.2, 0.8, 0.2, 1);
  }
  @keyframes slide {
    from {
      transform: translateY(100%);
    }
    to {
      transform: none;
    }
  }
  @keyframes rise {
    from {
      transform: translateY(12px);
      opacity: 0;
    }
    to {
      transform: none;
      opacity: 1;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .sheet,
    .rise {
      animation: none;
    }
  }
</style>
