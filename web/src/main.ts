import "@fontsource/inter/latin-400.css";
import "@fontsource/inter/latin-500.css";
import "@fontsource/inter/latin-600.css";
import "@fontsource/inter/latin-700.css";
import "@fontsource/jetbrains-mono/latin-400.css";
import "@fontsource/jetbrains-mono/latin-500.css";
import "./app.css";
import { mount } from "svelte";
import App from "./App.svelte";
import { watchInstallPrompt } from "./lib/install";

watchInstallPrompt();

mount(App, { target: document.getElementById("app")! });

// Push needs the service worker (src/sw.ts, built to /sw.js). vite dev doesn't serve it.
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch((err) => console.error("herdr-pocket: service worker", err));
}
