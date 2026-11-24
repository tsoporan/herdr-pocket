import { describe, expect, test } from "bun:test";
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
} from "./notify";

const ORIGIN = "https://pocket.example.ts.net";

const blocked = JSON.stringify({
  kind: "blocked",
  tag: "agent-w1:p2",
  title: "Refactor auth middleware",
  body: "Asks you a question · api. Tap to answer.",
  url: "/#agent=w1%3Ap2",
});

describe("parsePayload", () => {
  test("reads the server's payload", () => {
    expect(parsePayload(blocked)).toEqual({
      kind: "blocked",
      tag: "agent-w1:p2",
      title: "Refactor auth middleware",
      body: "Asks you a question · api. Tap to answer.",
      url: "/#agent=w1%3Ap2",
    });
  });

  test("refuses what the server doesn't send", () => {
    for (const t of [undefined, "", "not json", "null", "[]", "{}", '{"kind":"other"}', '{"kind":7}'])
      expect(parsePayload(t)).toBeNull();
  });

  test("missing strings become empty", () => {
    expect(parsePayload('{"kind":"test","title":5}')).toEqual({ kind: "test", tag: "", title: "", body: "", url: "" });
  });
});

describe("pushAction", () => {
  test("blocked shows a notification that buzzes again for the same agent", () => {
    expect(pushAction(parsePayload(blocked))).toEqual({
      do: "show",
      title: "Refactor auth middleware",
      options: {
        body: "Asks you a question · api. Tap to answer.",
        tag: "agent-w1:p2",
        renotify: true,
        icon: "/icon-192.png",
        badge: "/icon-monochrome-512.png",
        data: { url: "/#agent=w1%3Ap2", kind: "blocked" },
      },
    });
  });

  test("done replaces quietly", () => {
    const a = pushAction({ kind: "done", tag: "agent-w1:p2", title: "Fix test", body: "Finished · web", url: "/#agent=w1%3Ap2" });
    expect(a.do === "show" && a.options.renotify).toBe(false);
  });

  test("expiry and test open the app's root", () => {
    const a = pushAction({ kind: "expiry", tag: "expiry", title: "Tailscale sign-in ends in 7 days", body: "", url: "/" });
    expect(a.do === "show" && a.options.data).toEqual({ url: "/", kind: "expiry" });
  });

  test("renotify needs a tag, so an untagged blocked alert has neither", () => {
    const a = pushAction({ kind: "blocked", tag: "", title: "x", body: "", url: "/" });
    expect(a.do === "show" && "tag" in a.options).toBe(false);
    expect(a.do === "show" && a.options.renotify).toBe(false);
  });

  test("clear closes its tag and shows nothing", () => {
    expect(pushAction({ kind: "clear", tag: "agent-w1:p2", title: "", body: "", url: "" })).toEqual({
      do: "clear",
      tag: "agent-w1:p2",
    });
  });

  test("an unreadable push still shows something", () => {
    for (const p of [null, parsePayload('{"kind":"clear"}')]) {
      const a = pushAction(p);
      expect(a.do).toBe("show");
      expect(a.do === "show" && a.options.data).toEqual({ url: "/", kind: "test" });
    }
  });
});

const answer = { pane_id: "w1:p2", question_id: "q-7f3a", allow: "1", deny: "3" };

const permission = JSON.stringify({
  kind: "blocked",
  tag: "agent-w1:p2",
  title: "Refactor auth middleware",
  body: "Wants to run a command · api. Tap to see it first.",
  url: "/#agent=w1%3Ap2",
  answer,
});

describe("answer buttons", () => {
  test("a valid answer keeps it and gets the two buttons", () => {
    const p = parsePayload(permission);
    expect(p?.answer).toEqual(answer);
    expect(pushAction(p)).toEqual({
      do: "show",
      title: "Refactor auth middleware",
      options: {
        body: "Wants to run a command · api. Tap to see it first.",
        tag: "agent-w1:p2",
        renotify: true,
        icon: "/icon-192.png",
        badge: "/icon-monochrome-512.png",
        data: { url: "/#agent=w1%3Ap2", kind: "blocked", answer, name: "Refactor auth middleware" },
        actions: [
          { action: "allow", title: "Allow once" },
          { action: "deny", title: "Don’t allow" },
        ],
      },
    });
  });

  test("an invalid answer is treated as absent", () => {
    const bad: unknown[] = [
      null,
      "x",
      [],
      {},
      { ...answer, pane_id: "" },
      { ...answer, question_id: 7 },
      { ...answer, question_id: "q".repeat(257) },
      { ...answer, allow: "0" },
      { ...answer, allow: "12" },
      { ...answer, deny: 3 },
      { ...answer, deny: "a" },
      { ...answer, deny: "1" },
    ];
    for (const a of bad) {
      const p = parsePayload(JSON.stringify({ ...JSON.parse(permission), answer: a }));
      expect(p).not.toBeNull();
      expect(p && "answer" in p).toBe(false);
      const act = pushAction(p);
      expect(act.do === "show" && "actions" in act.options).toBe(false);
      expect(act.do === "show" && act.options.data).toEqual({ url: "/#agent=w1%3Ap2", kind: "blocked" });
    }
  });

  test("only a blocked alert carries buttons", () => {
    const p = parsePayload(JSON.stringify({ ...JSON.parse(permission), kind: "done" }));
    expect(p && "answer" in p).toBe(false);
    const a = pushAction({ kind: "done", tag: "t", title: "x", body: "", url: "/", answer });
    expect(a.do === "show" && "actions" in a.options).toBe(false);
  });
});

describe("answerRequest", () => {
  const data = { url: "/#agent=w1%3Ap2", kind: "blocked", answer, name: "Refactor auth middleware" };

  test("each button sends its digit", () => {
    expect(answerRequest("allow", data)).toEqual({ choice: "allow", body: { pane_id: "w1:p2", question_id: "q-7f3a", option: "1" } });
    expect(answerRequest("deny", data)).toEqual({ choice: "deny", body: { pane_id: "w1:p2", question_id: "q-7f3a", option: "3" } });
  });

  test("a plain tap, an unknown action, or no answer sends nothing", () => {
    expect(answerRequest("", data)).toBeNull();
    expect(answerRequest("always", data)).toBeNull();
    for (const d of [null, undefined, "x", { url: "/" }, { answer: { ...answer, allow: "x" } }]) expect(answerRequest("allow", d)).toBeNull();
  });
});

describe("answerOutcome", () => {
  test("reads the server's response", () => {
    expect(answerOutcome({ ok: true })).toBe("ok");
    expect(answerOutcome({ ok: false, code: "question_changed", message: "m" })).toBe("question_changed");
    expect(answerOutcome({ ok: false, code: "forbidden", message: "m" })).toBe("forbidden");
  });

  test("anything else is unreachable", () => {
    for (const b of [null, "ok", 1, [], {}, { ok: "true" }, { ok: false }, { ok: false, code: "" }]) expect(answerOutcome(b)).toBeNull();
  });
});

describe("answeredAction", () => {
  const data = { url: "/#agent=w1%3Ap2", kind: "blocked", answer, name: "Refactor auth middleware" };
  const replaced = (title: string, body: string, sent = false) => ({
    title,
    options: {
      body,
      tag: "agent-w1:p2",
      renotify: false,
      icon: "/icon-192.png",
      badge: "/icon-monochrome-512.png",
      data: { url: "/#agent=w1%3Ap2", kind: "blocked", ...(sent && { sent: true }) },
    },
  });

  test("while sending", () => {
    expect(sendingNotice(data, "agent-w1:p2")).toEqual(replaced("Refactor auth middleware", "Checking the screen, then sending…"));
  });

  test("allowed and denied", () => {
    expect(answeredAction("allow", "ok", data, "agent-w1:p2", "studio")).toEqual(
      replaced("Allowed once · Refactor auth middleware", "It’s working again.", true),
    );
    expect(answeredAction("deny", "ok", data, "agent-w1:p2", "studio")).toEqual(
      replaced("Didn’t allow · Refactor auth middleware", "It’s waiting for your next message.", true),
    );
  });

  test("the clear after a sent answer leaves its outcome up; it closes the rest", () => {
    expect(closesOnClear(answeredAction("allow", "ok", data, "agent-w1:p2", "studio").options.data)).toBe(false);
    for (const o of ["question_changed", null])
      expect(closesOnClear(answeredAction("allow", o, data, "agent-w1:p2", "studio").options.data)).toBe(true);
    expect(closesOnClear(sendingNotice(data, "agent-w1:p2").options.data)).toBe(true);
    for (const d of [data, null, undefined, {}, { sent: "true" }]) expect(closesOnClear(d)).toBe(true);
  });

  test("the question changed", () => {
    expect(answeredAction("allow", "question_changed", data, "agent-w1:p2", "studio")).toEqual(
      replaced("Didn’t send: the question changed", "Refactor auth middleware is asking something new. Tap to see it."),
    );
  });

  test("no reply says it may not have been sent", () => {
    expect(answeredAction("deny", null, data, "agent-w1:p2", "studio")).toEqual(
      replaced("No reply from studio", "It may not have been sent. Check Tailscale is on, then tap to see the agent."),
    );
  });

  test("a herdr error says it's not sure", () => {
    expect(answeredAction("allow", "herdr_error", data, "agent-w1:p2", "studio")).toEqual(
      replaced("Not sure it was sent · Refactor auth middleware", "herdr reported an error. Tap to see the agent."),
    );
  });

  test("a refusal by the server says it didn't send", () => {
    for (const o of ["not_found", "forbidden", "invalid_request", "read_only"])
      expect(answeredAction("deny", o, data, "agent-w1:p2", "studio")).toEqual(
        replaced("Didn’t send · Refactor auth middleware", "Tap to answer in the app."),
      );
  });

  test("no buttons, and no tag when there was none", () => {
    const n = answeredAction("allow", "ok", data, "", "studio");
    expect("actions" in n.options).toBe(false);
    expect("tag" in n.options).toBe(false);
  });
});

describe("hostLabel", () => {
  test("the hostname's first label", () => {
    expect(hostLabel("studio.example-tail.ts.net")).toBe("studio");
    expect(hostLabel("localhost")).toBe("localhost");
    expect(hostLabel("127.0.0.1")).toBe("127.0.0.1");
    expect(hostLabel("[::1]")).toBe("[::1]");
  });
});

describe("clickTarget", () => {
  test("opens the agent's deep link on this origin", () => {
    expect(clickTarget({ url: "/#agent=w1%3Ap2" }, ORIGIN)).toBe(`${ORIGIN}/#agent=w1%3Ap2`);
    expect(clickTarget({ url: "/" }, ORIGIN)).toBe(`${ORIGIN}/`);
  });

  test("anything else opens the root", () => {
    for (const d of [null, undefined, "x", {}, { url: 3 }, { url: "" }, { url: "https://evil.example/" }, { url: "//evil.example/x" }])
      expect(clickTarget(d, ORIGIN)).toBe(`${ORIGIN}/`);
  });
});
