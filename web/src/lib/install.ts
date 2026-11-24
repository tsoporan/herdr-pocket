// Installing the app from inside it. Chrome fires beforeinstallprompt once
// the app is installable; the saved event's prompt() then shows Chrome's own
// install dialog from a tap in Settings. It isn't prevented, so Chrome may
// still show its own install banner too.

interface InstallPromptEvent extends Event {
  prompt(): Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed" }>;
}

let saved: InstallPromptEvent | null = null;
const listeners = new Set<() => void>();

function changed() {
  for (const f of listeners) f();
}

/** Call once at startup: the event can fire before Settings is ever opened. */
export function watchInstallPrompt(): void {
  window.addEventListener("beforeinstallprompt", (e) => {
    saved = e as InstallPromptEvent;
    changed();
  });
  window.addEventListener("appinstalled", () => {
    saved = null;
    changed();
  });
}

/** True when running as the installed app rather than a browser tab. */
export function runningInstalled(): boolean {
  return matchMedia("(display-mode: standalone)").matches;
}

export function canInstall(): boolean {
  return saved !== null;
}

/** Calls f whenever canInstall may have changed; returns the unsubscribe. */
export function onInstallChange(f: () => void): () => void {
  listeners.add(f);
  return () => listeners.delete(f);
}

/** Shows Chrome's install dialog; true if the user accepted. The event is single-use. */
export async function install(): Promise<boolean> {
  const e = saved;
  if (!e) return false;
  saved = null;
  changed();
  await e.prompt();
  return (await e.userChoice).outcome === "accepted";
}
