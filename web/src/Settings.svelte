<script lang="ts">
  // Settings. There is no mockup; it follows
  // the mockups' idiom: a large title, inset grouped lists like the agent
  // view's Details, plain words, and the one accent. Every change saves
  // through push.subscribe once this phone is subscribed.
  import type { PushSettings, WebPushSubscription } from "./lib/protocol";
  import {
    DEFAULT_QUIET,
    DEFAULT_SETTINGS,
    DESK_MINUTES,
    deviceTimeZone,
    keyBytes,
    lostSubscription,
    pushOn,
    sameKey,
  } from "./lib/push";
  import { canInstall, install, onInstallChange, runningInstalled } from "./lib/install";
  import { LIST_HASH } from "./lib/route";
  import type { Socket } from "./lib/socket";
  import { deskCaption, deskLabel, pushErrorText, testPushText } from "./lib/words";

  let {
    socket,
    connected,
    vapidKey,
  }: {
    socket: Socket;
    connected: boolean;
    /** From the snapshot; null until one arrives. */
    vapidKey: string | null;
  } = $props();

  const installedApp = runningInstalled();
  let installable = $state(canInstall());
  $effect(() => onInstallChange(() => (installable = canInstall())));

  const supported = "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;

  let permission = $state<NotificationPermission>(supported ? Notification.permission : "denied");
  let subscription = $state.raw<PushSubscription | null>(null);
  /** The computer holds this phone's subscription: its last push.get or push.subscribe ack was ok. */
  let stored = $state(false);
  let loading = $state(supported);
  let subscribing = $state(false);

  let blocked = $state(DEFAULT_SETTINGS.blocked);
  let done = $state(DEFAULT_SETTINGS.done);
  let quietOn = $state(false);
  let quietStart = $state(DEFAULT_QUIET.start);
  let quietEnd = $state(DEFAULT_QUIET.end);
  let deskMinutes = $state(DEFAULT_SETTINGS.desk_minutes);

  let error = $state<string | null>(null);
  let testing = $state(false);
  let testResult = $state<string | null>(null);

  const timeZone = deviceTimeZone();
  const on = $derived(pushOn(permission, subscription !== null, stored));
  /** Settings can't take effect here: the browser can't get pushes, or Chrome blocks them. */
  const settingsOff = $derived(!supported || permission === "denied");

  function settings(): PushSettings {
    return {
      blocked,
      done,
      quiet: quietOn ? { start: quietStart, end: quietEnd, tz: timeZone } : null,
      desk_minutes: deskMinutes,
    };
  }

  function apply(s: PushSettings) {
    blocked = s.blocked;
    done = s.done;
    quietOn = s.quiet !== null;
    if (s.quiet) {
      quietStart = s.quiet.start;
      quietEnd = s.quiet.end;
    }
    deskMinutes = s.desk_minutes;
  }

  // Only the latest save's ack may set the screen, so a slow ack can't undo a later change.
  let saveSeq = 0;

  async function save() {
    if (!subscription) return;
    const seq = ++saveSeq;
    const r = await socket.request({
      type: "push.subscribe",
      subscription: subscription.toJSON() as WebPushSubscription,
      settings: settings(),
    });
    if (seq !== saveSeq) return;
    if (!r.ok) {
      if (lostSubscription(r.error.code)) stored = false;
      error = `Not saved. ${pushErrorText(r.error)}`;
      return;
    }
    stored = true;
    error = null;
    if (r.settings) apply(r.settings);
  }

  // Read this phone's subscription, then its settings from the computer.
  async function load(key: string) {
    error = null;
    try {
      const reg = await navigator.serviceWorker.getRegistration("/");
      let sub = (await reg?.pushManager.getSubscription()) ?? null;
      // A subscription made with another key can't get this computer's pushes.
      if (sub && !sameKey(sub.options.applicationServerKey, key)) {
        await sub.unsubscribe();
        sub = null;
      }
      subscription = sub;
      if (!sub) {
        stored = false;
        return;
      }
      const r = await socket.request({ type: "push.get", endpoint: sub.endpoint });
      if (r.ok) {
        stored = true;
        if (r.settings) apply(r.settings);
      }
      // The computer lost it (a new state dir, say): send it again with what's on screen.
      else if (r.error.code === "not_found") await save();
      else {
        if (lostSubscription(r.error.code)) stored = false;
        error = `Couldn't load the settings. ${pushErrorText(r.error)}`;
      }
    } catch (err) {
      error = `Couldn't read this phone's subscription: ${err instanceof Error ? err.message : String(err)}`;
    } finally {
      loading = false;
    }
  }

  // On open, and again after a reconnect or a new key.
  $effect(() => {
    if (supported && connected && vapidKey) void load(vapidKey);
  });

  // Allowing notifications in Chrome's site settings happens away from this page.
  $effect(() => {
    if (!supported) return;
    const onVisible = () => {
      if (document.visibilityState === "visible") permission = Notification.permission;
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  });

  async function turnOn() {
    if (!vapidKey) return;
    const key = vapidKey;
    error = null;
    // Asked straight from the tap: Chrome refuses a permission prompt without one.
    permission = await Notification.requestPermission();
    if (permission !== "granted") return;
    subscribing = true;
    try {
      const reg = await navigator.serviceWorker.ready;
      subscription = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(key) });
      await save();
    } catch (err) {
      error = `Couldn't turn them on: ${err instanceof Error ? err.message : String(err)}`;
    } finally {
      subscribing = false;
    }
  }

  async function sendTest() {
    if (!subscription) return;
    testing = true;
    testResult = null;
    const r = await socket.request({ type: "push.test", endpoint: subscription.endpoint });
    // A 404 or 410 from the push service deletes the subscription on the computer.
    if (!r.ok && lostSubscription(r.error.code)) stored = false;
    testResult = testPushText(r);
    testing = false;
  }

  // A cleared time field isn't a window; the last full one stays saved.
  function saveTimes() {
    if (quietStart && quietEnd) void save();
  }

  function toggle(set: (v: boolean) => void) {
    return (e: Event & { currentTarget: HTMLInputElement }) => {
      set(e.currentTarget.checked);
      void save();
    };
  }
</script>

<div class="view">
  <nav>
    <a href={LIST_HASH} class="back">
      <svg width="10" height="17" viewBox="0 0 10 17" aria-hidden="true"><path d="M8.5 1.5L2 8.5l6.5 7" stroke="#e6fb04" stroke-width="2" fill="none" stroke-linecap="round"></path></svg>
      Agents
    </a>
  </nav>

  <main>
    <h1>Settings</h1>

    {#if !installedApp}
      <section aria-labelledby="app">
        <h2 id="app">App</h2>
        <div class="group">
          {#if installable}
            <button type="button" class="row action" onclick={() => void install()}>Install herdr-pocket on this phone</button>
          {:else}
            <div class="row stack">
              <span class="label">Install from Chrome's menu</span>
              <span class="sub">Tap ⋮ at the top right, then Install app (or Add to home screen).</span>
            </div>
          {/if}
        </div>
        <p class="caption">Installed, it opens full screen from your home screen, like any other app.</p>
      </section>
    {/if}

    <section aria-labelledby="notifications">
      <h2 id="notifications">Notifications</h2>
      <div class="group">
        {#if !supported}
          <div class="row stack">
            <span class="label">This browser can't get pushes</span>
            <span class="sub">Open herdr-pocket in Chrome on the phone, at its https address.</span>
          </div>
        {:else if permission === "denied"}
          <div class="row stack">
            <span class="label">Blocked in Chrome</span>
            <span class="sub"
              >To allow them, open this site's settings in Chrome (the icon by the address, or ⋮ → App info in the installed app),
              then Notifications → Allow.</span
            >
          </div>
        {:else if on}
          <div class="row">
            <span class="label on">On for this phone</span>
          </div>
        {:else if loading}
          <div class="row">
            <span class="label muted">Checking this phone…</span>
          </div>
        {:else}
          <button type="button" class="row action" disabled={!connected || !vapidKey || subscribing} onclick={turnOn}>
            {subscribing ? "Turning on…" : "Turn on notifications"}
          </button>
        {/if}
      </div>
      {#if error}
        <p class="caption error" role="alert">{error}</p>
      {:else if supported && permission !== "denied" && !on && !loading}
        <p class="caption">Chrome asks first. Then this phone gets a push when an agent needs you, even with the app closed.</p>
      {/if}
    </section>

    <section aria-labelledby="when">
      <h2 id="when">When to notify</h2>
      <div class="group">
        <label class="row">
          <span class="label">When an agent needs an answer</span>
          <input
            type="checkbox"
            role="switch"
            name="blocked"
            class="switch"
            checked={blocked}
            disabled={settingsOff}
            onchange={toggle((v) => (blocked = v))}
          />
        </label>
        <label class="row">
          <span class="label">When an agent finishes</span>
          <input
            type="checkbox"
            role="switch"
            name="done"
            class="switch"
            checked={done}
            disabled={settingsOff}
            onchange={toggle((v) => (done = v))}
          />
        </label>
      </div>
    </section>

    <section aria-labelledby="quiet">
      <h2 id="quiet">Quiet hours</h2>
      <div class="group">
        <label class="row">
          <span class="label">Hold pushes between set times</span>
          <input
            type="checkbox"
            role="switch"
            name="quiet"
            class="switch"
            checked={quietOn}
            disabled={settingsOff}
            onchange={toggle((v) => (quietOn = v))}
          />
        </label>
        {#if quietOn}
          <label class="row">
            <span class="label">From</span>
            <input type="time" name="quiet-start" class="field" required disabled={settingsOff} bind:value={quietStart} onchange={saveTimes} />
          </label>
          <label class="row">
            <span class="label">To</span>
            <input type="time" name="quiet-end" class="field" required disabled={settingsOff} bind:value={quietEnd} onchange={saveTimes} />
          </label>
        {/if}
      </div>
      <p class="caption">
        In this phone's time zone, {timeZone}. A push held in quiet hours goes out when they end, if the agent still needs you.
      </p>
    </section>

    <section aria-labelledby="desk">
      <h2 id="desk">At the desk</h2>
      <div class="group">
        <label class="row">
          <span class="label">Not while I'm at that agent on the computer</span>
          <select
            name="desk-minutes"
            class="field"
            value={deskMinutes}
            disabled={settingsOff}
            onchange={(e) => {
              deskMinutes = Number(e.currentTarget.value);
              void save();
            }}
          >
            {#each DESK_MINUTES as m (m)}
              <option value={m}>{deskLabel(m)}</option>
            {/each}
          </select>
        </label>
      </div>
      <p class="caption">{deskCaption(deskMinutes)}</p>
    </section>

    <section aria-label="Test">
      <div class="group">
        <button type="button" class="row action" disabled={!on || !connected || testing} onclick={sendTest}>
          {testing ? "Sending…" : "Send a test push"}
        </button>
      </div>
      {#if testResult}
        <p class="caption" role="status">{testResult}</p>
      {:else if !on}
        <p class="caption">Turn on notifications first.</p>
      {/if}
      <p class="caption">
        Pushes go through Google's push service, encrypted. They show the agent's name and workspace, never what it's doing.
      </p>
    </section>

    <section aria-labelledby="about">
      <h2 id="about">About</h2>
      <div class="group">
        <div class="row">
          <span class="label">Version</span>
          <span class="value">{__APP_VERSION__}</span>
        </div>
        <div class="row">
          <span class="label">Address</span>
          <span class="value">{location.host}</span>
        </div>
      </div>
    </section>
  </main>
</div>

<style>
  .view {
    min-height: 100dvh;
  }
  nav {
    display: flex;
    align-items: center;
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
  main {
    box-sizing: border-box;
    padding: 6px 16px 32px;
  }
  h1 {
    margin: 0 4px 18px;
    font-size: 34px;
    font-weight: 700;
    letter-spacing: -0.02em;
  }
  section {
    margin-bottom: 26px;
  }
  h2 {
    margin: 0 4px 8px;
    font-size: 15px;
    font-weight: 600;
    color: #f5f5f7;
  }
  .group {
    background: #1c1c1e;
    border-radius: 14px;
    overflow: hidden;
  }
  /* A row as in the agent view's Details: 50px tall, 16px in, a hairline between rows. */
  .row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    min-height: 50px;
    padding: 0 16px;
    box-sizing: border-box;
    width: 100%;
    margin: 0;
    border: 0;
    background: none;
    color: #f5f5f7;
    font: inherit;
    text-align: left;
  }
  .row + .row {
    border-top: 0.5px solid #38383a;
  }
  .row.stack {
    flex-direction: column;
    align-items: flex-start;
    justify-content: center;
    gap: 4px;
    padding-top: 12px;
    padding-bottom: 12px;
  }
  .label {
    font-size: 16px;
    line-height: 1.3;
    padding: 8px 0;
  }
  .stack .label {
    padding: 0;
  }
  .label.on {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .label.on::before {
    content: "";
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: #30d158;
  }
  .label.muted {
    color: #8e8e93;
  }
  .sub {
    font-size: 14px;
    line-height: 1.4;
    color: #a1a1a6;
  }
  .value {
    font-size: 15px;
    color: #a1a1a6;
    text-align: right;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .action {
    font-size: 16px;
    font-weight: 500;
    color: #e6fb04;
    cursor: pointer;
  }
  .action:disabled {
    color: #8e8e93;
    cursor: default;
  }
  .caption {
    margin: 8px 4px 0;
    font-size: 13px;
    line-height: 1.4;
    color: #8e8e93;
  }
  .caption.error {
    color: #ffb340;
  }
  /* A native-looking switch: the accent track when on. No slide: SPEC UX 6 keeps motion to spinners and sheets. */
  .switch {
    appearance: none;
    flex: 0 0 auto;
    position: relative;
    width: 51px;
    height: 31px;
    margin: 0;
    border-radius: 16px;
    background: #3a3a3c;
    cursor: pointer;
  }
  .switch::after {
    content: "";
    position: absolute;
    top: 2px;
    left: 2px;
    width: 27px;
    height: 27px;
    border-radius: 50%;
    background: #f5f5f7;
    box-shadow: 0 2px 4px rgba(0, 0, 0, 0.4);
  }
  .switch:checked {
    background: #e6fb04;
  }
  .switch:checked::after {
    transform: translateX(20px);
  }
  .switch:disabled,
  .field:disabled {
    opacity: 0.4;
    cursor: default;
  }
  .switch:focus-visible,
  .field:focus-visible {
    outline: 2px solid #e6fb04;
    outline-offset: 2px;
  }
  .field {
    flex: 0 0 auto;
    min-height: 36px;
    margin: 0;
    padding: 0 10px;
    border: 0;
    border-radius: 8px;
    background: #2c2c2e;
    color: #f5f5f7;
    color-scheme: dark;
    font-family: inherit;
    font-size: 15px;
  }
</style>
