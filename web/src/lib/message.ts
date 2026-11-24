// The Markdown of an agent's last message,
// parsed into blocks and spans the view renders as text: headings, paragraphs,
// lists (nested, mixed, task boxes), quotes, code fences, tables, rules, and
// inline `code`, **bold**, *italic*, ~~strike~~ and [links](…) (their text
// only; nothing is tappable). HTML is never interpreted: `<b>` stays text.

/** A run of text with its inline styles. */
export interface Span {
  text: string;
  code?: boolean;
  bold?: boolean;
  italic?: boolean;
  strike?: boolean;
  link?: boolean;
}

/** A list item; depth 0 is the outer list, nesting is capped at 3. */
export interface Item {
  depth: number;
  /** "•" for a bullet, "3." for a numbered item, "☐" / "☑" for a task. */
  marker: string;
  spans: Span[];
}

export type Block =
  | { kind: "p"; lines: Span[][] }
  | { kind: "h"; level: number; spans: Span[] }
  | { kind: "list"; items: Item[] }
  | { kind: "quote"; lines: Span[][] }
  | { kind: "pre"; lang: string; text: string }
  | { kind: "table"; head: Span[][]; rows: Span[][][] }
  | { kind: "hr" };

type Style = Omit<Span, "text">;

// One alternative per inline form, tried left to right. Emphasis needs
// non-space just inside its markers, so "2 * 3 * 4" stays text, and "_" must
// not touch a word on the outside, so snake_case_names stay text.
const INLINE = new RegExp(
  [
    /(`+)(?!`)([^\n]*?[^`\n])\1(?!`)/.source, // 1, 2: code
    /\[([^\]\n]+)\]\(([^()\s]*)(?:\s+"[^"\n]*")?\)/.source, // 3: link text
    /\*\*(?=\S)(.+?)(?<=\S)\*\*(?!\*)/.source, // 5: bold; "***" closes with its last two
    /(?<!\w)__(?=\S)(.+?)(?<=\S)__(?!\w)/.source, // 6: bold
    /\*(?=[^\s*])(.+?)(?<=[^\s*])\*/.source, // 7: italic
    /(?<!\w)_(?=[^\s_])(.+?)(?<=[^\s_])_(?!\w)/.source, // 8: italic
    /~~(?=\S)(.+?)(?<=\S)~~/.source, // 9: strike
  ].join("|"),
  "g",
);

/** Splits one line into styled spans. A marker without a partner stays text. */
export function spans(line: string, style: Style = {}): Span[] {
  const out: Span[] = [];
  const text = (s: string) => {
    if (!s) return;
    const last = out[out.length - 1];
    if (last && sameStyle(last, style)) last.text += s;
    else out.push({ text: s, ...style });
  };
  let at = 0;
  for (const m of line.matchAll(INLINE)) {
    text(line.slice(at, m.index));
    at = m.index + m[0].length;
    if (m[2] !== undefined) {
      // CommonMark: one space either side of a code span is padding.
      const code = /^ .* $/.test(m[2]) && m[2].trim() ? m[2].slice(1, -1) : m[2];
      out.push({ text: code, ...style, code: true });
    } else {
      const [inner, add]: [string, Style] =
        m[3] !== undefined
          ? [m[3], { link: true }]
          : m[5] !== undefined || m[6] !== undefined
            ? [(m[5] ?? m[6])!, { bold: true }]
            : m[7] !== undefined || m[8] !== undefined
              ? [(m[7] ?? m[8])!, { italic: true }]
              : [m[9]!, { strike: true }];
      for (const s of spans(inner, { ...style, ...add })) {
        const last = out[out.length - 1];
        if (last && !s.code && !last.code && sameStyle(last, s)) last.text += s.text;
        else out.push(s);
      }
    }
  }
  text(line.slice(at));
  return out;
}

function sameStyle(a: Style, b: Style): boolean {
  return !a.code && !b.code && !a.bold === !b.bold && !a.italic === !b.italic && !a.strike === !b.strike && !a.link === !b.link;
}

/**
 * Splits an asking message into its body and the question it ends with: the
 * last paragraph (after the last blank line), else the last line. The
 * question is shown in full above the reply buttons, so the buttons never
 * answer text that is clamped out of view.
 */
export function splitQuestion(text: string): { body: string; question: string } {
  const t = text.trim();
  const para = t.lastIndexOf("\n\n");
  const cut = para >= 0 ? para : t.lastIndexOf("\n");
  if (cut < 0) return { body: "", question: t };
  return { body: t.slice(0, cut).trim(), question: t.slice(cut).trim() };
}

const FENCE = /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)/;
const HEADING = /^ {0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$/;
const RULE = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const QUOTE = /^ {0,3}>\s?(.*)$/;
const BULLET = /^(\s*)[-*+]\s+(.*)$/;
const NUMBERED = /^(\s*)(\d{1,9})[.)]\s+(.*)$/;
const TASK = /^\[([ xX])\]\s+(.*)$/;
const TABLE_RULE = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;

/** A table row's cells: split on "|" not escaped or in code, outer pipes dropped. */
function cells(line: string): Span[][] {
  let s = line.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
  const out: string[] = [];
  let cur = "";
  let tick = false;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i]!;
    if (ch === "\\" && s[i + 1] === "|") {
      cur += "|";
      i++;
    } else if (ch === "`") {
      tick = !tick;
      cur += ch;
    } else if (ch === "|" && !tick) {
      out.push(cur);
      cur = "";
    } else cur += ch;
  }
  out.push(cur);
  return out.map((c) => spans(c.trim()));
}

/**
 * The message as blocks. A blank line ends a paragraph, list or quote; a
 * paragraph keeps its line breaks; an indented line under a list item
 * continues it; an unclosed fence runs to the end (a bounded message may be
 * cut inside one).
 */
export function formatMessage(text: string): Block[] {
  const blocks: Block[] = [];
  let cur: Block | null = null;
  // The open list's items as raw text, parsed into spans once each is complete.
  let raw: { depth: number; marker: string; text: string }[] = [];
  let indents: number[] = [];
  let fence: { mark: string; lang: string; indent: number; lines: string[] } | null = null;

  const close = () => {
    if (cur?.kind === "list") cur.items = raw.map((r) => ({ depth: r.depth, marker: r.marker, spans: spans(r.text) }));
    if (cur) blocks.push(cur);
    cur = null;
    raw = [];
    indents = [];
  };
  const item = (indent: number, marker: string, body: string) => {
    if (cur?.kind !== "list") {
      close();
      cur = { kind: "list", items: [] };
    }
    // Depth by indent: deeper than the open item nests, back to an earlier indent returns to its level.
    while (indents.length && indent < indents[indents.length - 1]!) indents.pop();
    if (!indents.length || indent > indents[indents.length - 1]!) indents.push(indent);
    const task = TASK.exec(body);
    raw.push({
      depth: Math.min(indents.length - 1, 3),
      marker: task ? (task[1] === " " ? "☐" : "☑") : marker,
      text: task ? task[2]! : body,
    });
  };

  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!;
    if (fence) {
      const end = new RegExp(`^ {0,3}${fence.mark[0] === "`" ? "`" : "~"}{${fence.mark.length},}\\s*$`);
      if (end.test(line)) {
        blocks.push({ kind: "pre", lang: fence.lang, text: fence.lines.join("\n") });
        fence = null;
      } else {
        fence.lines.push(line.replace(new RegExp(`^ {0,${fence.indent}}`), ""));
      }
      continue;
    }
    if (line.trim() === "") {
      close();
      continue;
    }
    if (cur?.kind === "table") {
      if (line.includes("|")) {
        cur.rows.push(cells(line));
        continue;
      }
      close();
    }

    const f = FENCE.exec(line);
    if (f) {
      close();
      fence = { mark: f[1]!, lang: f[2]!, indent: line.length - line.trimStart().length, lines: [] };
      continue;
    }
    const h = HEADING.exec(line);
    if (h) {
      close();
      blocks.push({ kind: "h", level: h[1]!.length, spans: spans(h[2]!) });
      continue;
    }
    if (RULE.test(line)) {
      close();
      blocks.push({ kind: "hr" });
      continue;
    }
    const next = lines[i + 1];
    if (line.includes("|") && next !== undefined && next.includes("-") && TABLE_RULE.test(next)) {
      close();
      cur = { kind: "table", head: cells(line), rows: [] };
      i++;
      continue;
    }
    const q = QUOTE.exec(line);
    if (q) {
      if (cur?.kind !== "quote") {
        close();
        cur = { kind: "quote", lines: [] };
      }
      cur.lines.push(spans(q[1]!.trimEnd()));
      continue;
    }
    const b = BULLET.exec(line);
    if (b) {
      item(b[1]!.length, "•", b[2]!);
      continue;
    }
    const n = NUMBERED.exec(line);
    if (n) {
      item(n[1]!.length, `${Number(n[2])}.`, n[3]!);
      continue;
    }
    // (A cast: TS can't see that item() above opened the list across loop turns.)
    if ((cur as Block | null)?.kind === "list" && /^\s/.test(line)) {
      raw[raw.length - 1]!.text += ` ${line.trim()}`;
      continue;
    }
    if (cur?.kind !== "p") {
      close();
      cur = { kind: "p", lines: [] };
    }
    cur.lines.push(spans(line.trimEnd()));
  }
  close();
  if (fence) blocks.push({ kind: "pre", lang: fence.lang, text: fence.lines.join("\n") });
  return blocks;
}
