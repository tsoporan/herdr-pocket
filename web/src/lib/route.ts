// Hash routing: "#agent=<pane_id>" is an agent, SETTINGS_HASH is Settings,
// NEW_HASH is New task; anything else, such as no hash or LIST_HASH, is the
// Agents list.

export type Route = { view: "list" } | { view: "settings" } | { view: "new" } | { view: "agent"; paneId: string };

export const SETTINGS_HASH = "#settings";
export const NEW_HASH = "#/new";

export function parseRoute(hash: string): Route {
  if (hash === SETTINGS_HASH) return { view: "settings" };
  if (hash === NEW_HASH) return { view: "new" };
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  const paneId = params.get("agent");
  return paneId ? { view: "agent", paneId } : { view: "list" };
}

export const LIST_HASH = "#agents";

export function agentHash(paneId: string): string {
  return `#${new URLSearchParams({ agent: paneId })}`;
}
