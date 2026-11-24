// The app's side of push: the default settings, the VAPID key, and which
// shown notifications are stale. Pure functions.

import type { Agents } from "./agents";
import type { PushSettings } from "./protocol";

export const DEFAULT_SETTINGS: PushSettings = { blocked: true, done: false, quiet: null, desk_minutes: 5 };

export const DEFAULT_QUIET = { start: "22:00", end: "07:00" };

/** The choices for desk suppression, in minutes; 0 is off. */
export const DESK_MINUTES = [0, 2, 5, 10, 15];

/** The VAPID public key (base64url, as the snapshot has it) as the bytes pushManager.subscribe takes. */
export function keyBytes(base64url: string): Uint8Array<ArrayBuffer> {
  const b64 = base64url.replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(b64 + "=".repeat((4 - (b64.length % 4)) % 4));
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}

/** Whether a subscription was made with this key; one made with an old key must be replaced. */
export function sameKey(subscribed: ArrayBuffer | null, base64url: string): boolean {
  if (!subscribed) return false;
  const a = new Uint8Array(subscribed);
  const b = keyBytes(base64url);
  return a.length === b.length && a.every((x, i) => x === b[i]);
}

/**
 * Whether pushes are on for this phone: Chrome allows them, the phone has a
 * subscription, and the computer said it holds it (an ok push.get or
 * push.subscribe ack). A subscription the computer refused or lost is off.
 */
export function pushOn(permission: NotificationPermission, subscribed: boolean, stored: boolean): boolean {
  return permission === "granted" && subscribed && stored;
}

/**
 * Whether a failed push request means the computer doesn't hold this
 * phone's subscription. Other failures (a timeout, refused settings) leave
 * the stored one as it was.
 */
export function lostSubscription(code: string): boolean {
  return code === "not_found" || code === "forbidden";
}

/** The pane a notification tag names ("agent-<pane_id>"), or null. */
export function tagPane(tag: string): string | null {
  return tag.startsWith("agent-") && tag.length > 6 ? tag.slice(6) : null;
}

/**
 * The shown notifications to close: a blocked alert whose agent isn't
 * blocked any more (or has gone), and a done alert whose agent isn't done.
 * agents must come from a snapshot; an empty list closes every agent alert.
 */
export function staleNotifications<N extends { tag: string; data: unknown }>(shown: readonly N[], agents: Agents): N[] {
  return shown.filter((n) => {
    const pane = tagPane(n.tag);
    if (!pane) return false;
    const kind = typeof n.data === "object" && n.data !== null ? (n.data as Record<string, unknown>).kind : undefined;
    const status = agents[pane]?.status;
    if (kind === "blocked") return status !== "blocked";
    if (kind === "done") return status !== "done";
    return false;
  });
}

/** The device's IANA time zone, sent with quiet hours. */
export function deviceTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}
