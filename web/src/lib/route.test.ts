import { expect, test } from "bun:test";
import { LIST_HASH, NEW_HASH, SETTINGS_HASH, agentHash, parseRoute } from "./route";

test("parseRoute", () => {
  expect(parseRoute("")).toEqual({ view: "list" });
  expect(parseRoute("#")).toEqual({ view: "list" });
  expect(parseRoute(LIST_HASH)).toEqual({ view: "list" });
  expect(parseRoute("#agent=")).toEqual({ view: "list" });
  expect(parseRoute("#agent=w1:p1")).toEqual({ view: "agent", paneId: "w1:p1" });
  expect(parseRoute("#agent=w1%3Ap1")).toEqual({ view: "agent", paneId: "w1:p1" });
  expect(parseRoute(SETTINGS_HASH)).toEqual({ view: "settings" });
  expect(parseRoute(NEW_HASH)).toEqual({ view: "new" });
});

test("agentHash round-trips through parseRoute", () => {
  for (const id of ["w1:p1", "w10:p2", "a b&c=d#e"]) {
    expect(parseRoute(agentHash(id))).toEqual({ view: "agent", paneId: id });
  }
});
