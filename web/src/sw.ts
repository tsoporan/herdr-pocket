// The service worker, served at /sw.js with scope /. It handles pushes and taps on their notifications only: no fetch
// handler and no caching (that's M6). A tap on Allow once or Don't allow
// POSTs /api/answer. Type-checked by
// tsconfig.sw.json.

import {
  answeredAction,
  answerOutcome,
  answerRequest,
  clickTarget,
  closesOnClear,
  hostLabel,
  parsePayload,
  pushAction,
  sendingNotice,
} from "./lib/notify";
import type { AnswerOutcome } from "./lib/notify";

declare const self: ServiceWorkerGlobalScope;

// How long a tap waits for POST /api/answer. The server bounds its herdr
// calls well inside this.
const answerTimeoutMs = 15_000;

// A new version takes over at once, and controls the open windows so a tap can navigate them.
self.addEventListener("install", () => void self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

self.addEventListener("push", (event) => {
  const action = pushAction(parsePayload(event.data?.text()));
  event.waitUntil(
    (async () => {
      if (action.do === "show") return self.registration.showNotification(action.title, action.options);
      for (const n of await self.registration.getNotifications({ tag: action.tag })) if (closesOnClear(n.data)) n.close();
    })(),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const request = answerRequest(event.action, event.notification.data);
  if (request) {
    const { data, tag } = event.notification;
    event.waitUntil(
      (async () => {
        const sending = sendingNotice(data, tag);
        await self.registration.showNotification(sending.title, sending.options);
        let outcome: AnswerOutcome = null;
        try {
          const res = await fetch("/api/answer", {
            method: "POST",
            credentials: "same-origin",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(request.body),
            // A hung node must not leave "sending" up until the browser
            // ends the worker; the abort is shown as "Couldn't reach".
            signal: AbortSignal.timeout(answerTimeoutMs),
          });
          outcome = answerOutcome(await res.json());
        } catch {
          // Offline, timed out, or a response that isn't JSON: shown as
          // "Couldn't reach".
        }
        const n = answeredAction(request.choice, outcome, data, tag, hostLabel(self.location.hostname));
        await self.registration.showNotification(n.title, n.options);
      })(),
    );
    return;
  }
  const url = clickTarget(event.notification.data, self.location.origin);
  event.waitUntil(
    (async () => {
      const [win] = await self.clients.matchAll({ type: "window" });
      if (!win) return void (await self.clients.openWindow(url));
      // Focus first, while the tap still counts as the user's.
      const focused = await win.focus();
      await focused.navigate(url);
    })(),
  );
});
