import { describe, expect, test } from "bun:test";
import { formatMessage, spans, splitQuestion, type Span } from "./message";

const t = (text: string) => ({ text });
const c = (text: string) => ({ text, code: true });
const b = (text: string) => ({ text, bold: true });
const it = (text: string) => ({ text, italic: true });
const li = (marker: string, spans: Span[], depth = 0) => ({ depth, marker, spans });

test("paragraphs split on blank lines and keep their line breaks", () => {
  expect(formatMessage("The tests pass.\nThe branch is pushed.\n\n\nWant a PR?")).toEqual([
    { kind: "p", lines: [[t("The tests pass.")], [t("The branch is pushed.")]] },
    { kind: "p", lines: [[t("Want a PR?")]] },
  ]);
});

test("- * + bullets make one list, and an indented line continues an item", () => {
  expect(formatMessage("What I changed:\n- The test waits\n  for the cart.\n* It passed 50 times.\n+ Plus")).toEqual([
    { kind: "p", lines: [[t("What I changed:")]] },
    { kind: "list", items: [li("•", [t("The test waits for the cart.")]), li("•", [t("It passed 50 times.")]), li("•", [t("Plus")])] },
  ]);
});

test("numbered items keep their numbers, and mixed lists nest by indent", () => {
  expect(formatMessage("3. Run it\n   - with --race\n     - twice\n4) Merge it\n\nDone.")).toEqual([
    {
      kind: "list",
      items: [li("3.", [t("Run it")]), li("•", [t("with --race")], 1), li("•", [t("twice")], 2), li("4.", [t("Merge it")])],
    },
    { kind: "p", lines: [[t("Done.")]] },
  ]);
});

test("task boxes", () => {
  expect(formatMessage("- [x] tests\n- [ ] docs")).toEqual([{ kind: "list", items: [li("☑", [t("tests")]), li("☐", [t("docs")])] }]);
});

test("headings, rules and quotes", () => {
  expect(formatMessage("## What changed ##\nText\n\n---\n> quoted **line**\n> two")).toEqual([
    { kind: "h", level: 2, spans: [t("What changed")] },
    { kind: "p", lines: [[t("Text")]] },
    { kind: "hr" },
    { kind: "quote", lines: [[t("quoted "), b("line")], [t("two")]] },
  ]);
  expect(formatMessage("#hashtag")).toEqual([{ kind: "p", lines: [[t("#hashtag")]] }]);
});

test("code fences keep their text as is, and an unclosed fence runs to the end", () => {
  expect(formatMessage("Run:\n```bash\njust test\n\n  **not bold** <b>\n```\nAfter")).toEqual([
    { kind: "p", lines: [[t("Run:")]] },
    { kind: "pre", lang: "bash", text: "just test\n\n  **not bold** <b>" },
    { kind: "p", lines: [[t("After")]] },
  ]);
  expect(formatMessage("```\ncut off")).toEqual([{ kind: "pre", lang: "", text: "cut off" }]);
});

test("tables", () => {
  expect(formatMessage("| File | Change |\n|---|:--:|\n| `a.ts` | **new** |\n| b \\| c | x |\nafter")).toEqual([
    {
      kind: "table",
      head: [[t("File")], [t("Change")]],
      rows: [
        [[c("a.ts")], [b("new")]],
        [[t("b | c")], [t("x")]],
      ],
    },
    { kind: "p", lines: [[t("after")]] },
  ]);
  expect(formatMessage("a | b")).toEqual([{ kind: "p", lines: [[t("a | b")]] }]);
});

test("code spans", () => {
  expect(spans("Run `bun test` then `just build`.")).toEqual([t("Run "), c("bun test"), t(" then "), c("just build"), t(".")]);
  expect(spans("`whole`")).toEqual([c("whole")]);
  expect(spans("``a ` b``")).toEqual([c("a ` b")]);
  expect(spans("`**x**`")).toEqual([c("**x**")]);
});

test("a marker without a partner, and an empty pair, stay text", () => {
  expect(spans("it's a ` mark")).toEqual([t("it's a ` mark")]);
  expect(spans("``")).toEqual([t("``")]);
  expect(spans("```ts")).toEqual([t("```ts")]);
  expect(spans("2 * 3 * 4 and a ** b")).toEqual([t("2 * 3 * 4 and a ** b")]);
  expect(spans("snake_case_name and __init__.py")).toEqual([t("snake_case_name and "), b("init"), t(".py")]);
});

test("bold, italic, strike and links, nested", () => {
  expect(spans("**Done.** It's *really* ~~not~~ fine")).toEqual([
    b("Done."),
    t(" It's "),
    it("really"),
    t(" "),
    { text: "not", strike: true },
    t(" fine"),
  ]);
  expect(spans("**bold with `code` and *em***")).toEqual([
    b("bold with "),
    { text: "code", bold: true, code: true },
    b(" and "),
    { text: "em", bold: true, italic: true },
  ]);
  expect(spans("***both***")).toEqual([{ text: "both", bold: true, italic: true }]);
  expect(spans("See [the PR](https://example.test/pr/1).")).toEqual([t("See "), { text: "the PR", link: true }, t(".")]);
  expect(spans("_quiet_ word")).toEqual([it("quiet"), t(" word")]);
});

test("HTML stays text", () => {
  expect(formatMessage("<b>bold</b> and <script>x()</script>")).toEqual([
    { kind: "p", lines: [[t("<b>bold</b> and <script>x()</script>")]] },
  ]);
  expect(spans("`<img src=x onerror=alert(1)>`")).toEqual([c("<img src=x onerror=alert(1)>")]);
});

test("**bold** is not a bullet, and CRLF line ends split like LF", () => {
  expect(formatMessage("**Done.**\r\nNext?")).toEqual([{ kind: "p", lines: [[b("Done.")], [t("Next?")]] }]);
});

test("an empty message has no blocks", () => {
  expect(formatMessage("")).toEqual([]);
  expect(formatMessage("\n  \n")).toEqual([]);
});

describe("splitQuestion", () => {
  test("the last paragraph is the question", () => {
    expect(splitQuestion("Done.\n\n- a\n- b\n\nWant me to push?")).toEqual({ body: "Done.\n\n- a\n- b", question: "Want me to push?" });
  });
  test("without paragraphs, the last line", () => {
    expect(splitQuestion("Tests pass.\nOpen a PR?")).toEqual({ body: "Tests pass.", question: "Open a PR?" });
  });
  test("a one-line message is all question", () => {
    expect(splitQuestion("  Ship it?  ")).toEqual({ body: "", question: "Ship it?" });
  });
});
