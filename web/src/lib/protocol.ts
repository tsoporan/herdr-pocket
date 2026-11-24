// The WebSocket contract between the app and the server. Every wire shape lives here; nothing else declares message types.

export type Status = "idle" | "working" | "blocked" | "done" | "unknown";

export interface Agent {
  pane_id: string;
  /** The name shown: herdr's, else the terminal title, else the kind. */
  name: string;
  /** herdr's own name; absent when it has none. */
  herdr_name?: string;
  agent: string;
  status: Status;
  line: string;
  workspace_id: string;
  workspace_label: string;
  tab_id: string;
  changed_at: string; // RFC3339
  state_change_seq: number; // herdr's recency counter; breaks changed_at ties
  tokens: Record<string, string>;
  focused: boolean;
}

export interface Workspace {
  workspace_id: string;
  label: string;
  number: number;
  /** The most common folder among its agents; "" when it has none (a new tab then opens in its current folder). Absent in old servers. */
  path?: string;
  agent_count?: number;
}

/** A controller can send input; a reader can only view. */
export type Role = "controller" | "reader";

/** The node's Tailscale key expiry; the server sends it only within 14 days of the date. */
export interface Expiry {
  at: string; // RFC3339
  days_left: number; // whole days, 0 or less once it has ended
}

/** One device's push settings. */
export interface PushSettings {
  blocked: boolean;
  done: boolean;
  /** Off, or a window in the device's IANA time zone; times are "HH:MM", and end may be before start (across midnight). */
  quiet: { start: string; end: string; tz: string } | null;
  /** How long a herdr focus counts as being at the desk; 0 turns suppression off. */
  desk_minutes: number;
}

/** PushSubscription.toJSON(), as the browser gives it. */
export interface WebPushSubscription {
  endpoint: string;
  expirationTime?: number | null;
  keys: { p256dh: string; auth: string };
}

/** The decrypted push payload the service worker gets. It never carries prompt or output text. */
export interface PushPayload {
  kind: "blocked" | "done" | "clear" | "expiry" | "test";
  tag: string; // "agent-<pane_id>" for an agent
  title: string;
  body: string;
  url: string; // "/#agent=<pane_id>", or "/"
  answer?: PushAnswer; // blocked only: a Bash permission prompt a controller can answer from the notification
}

/** What a notification button sends. Option keys only, never labels. */
export interface PushAnswer {
  pane_id: string;
  question_id: string;
  allow: string; // the "Allow once" option's digit
  deny: string; // the "Don't allow" option's digit
}

/** POST /api/answer's body. option is the answer's allow or deny digit. */
export interface AnswerRequest {
  pane_id: string;
  question_id: string;
  option: string;
}

/** Who a lined-up item is for: one agent, or the next free agent in a workspace. */
export type QueueTarget = { pane_id: string } | { workspace_id: string };

export type QueueState = "waiting" | "sent" | "picked_up" | "stalled" | "failed" | "dropped";

/** A message lined up for when an agent is done. Times are RFC3339. */
export interface QueueItem {
  id: string;
  target: QueueTarget;
  text: string;
  by: string; // the login that added it
  added: string;
  state: QueueState;
  sent_at?: string;
  picked_up_at?: string;
  sent_to?: string; // the pane a workspace item went to
  error?: string; // herdr's error code when it refused it, such as agent_blocked
  ended_at?: string; // when it reached a finished state
}

/** An agent kind the server lets the app start, with the app's name for it. */
export interface StartKind {
  kind: string;
  label: string;
}

export type StartState = "running" | "done" | "asks_first" | "stalled" | "failed" | "unsure";

/** The step a start job got to; the times on the job say which are done. */
export type StartStep = "opened" | "started" | "sent";

/**
 * A start job: open a tab, start the agent, send the task. It never carries
 * the task text; the app keeps that in its draft. Times are RFC3339.
 */
export interface StartJob {
  id: string; // the app-made job_id
  by: string; // the login that started it
  workspace_id: string;
  kind: string;
  name: string;
  cwd?: string;
  pane_id?: string;
  tab_id?: string;
  state: StartState;
  began: string;
  opened_at?: string;
  started_at?: string;
  sent_at?: string;
  ended_at?: string;
  /** herdr's error code, lost_pane or unsure; never herdr's text. */
  error?: string;
  failed_step?: StartStep;
}

// server → app

export interface Snapshot {
  type: "snapshot";
  host: string;
  role: Role;
  expiry: Expiry | null;
  push: { vapid_public_key: string };
  /** The server's app bundle; absent in old servers and in tests. */
  build?: string;
  agents: Agent[];
  workspaces: Workspace[];
  /** Unfinished items plus those finished in the last 24 h; absent or null in old servers and tests. */
  queue?: QueueItem[] | null;
  /** The kinds the app may start; absent or empty means starting is off. */
  start_kinds?: StartKind[] | null;
  /** Every login's start jobs: running, plus those finished in the last hour (at most 20). */
  starts?: StartJob[] | null;
}

/**
 * A null value in either map means the record was removed. Every field may be
 * absent, meaning unchanged; expiry null means the banner goes away.
 */
export interface Patch {
  type: "patch";
  agents?: Record<string, Agent | null>;
  workspaces?: Record<string, Workspace | null>;
  role?: Role;
  expiry?: Expiry | null;
  /** The whole list when it changed; null means empty. */
  queue?: QueueItem[] | null;
  /** The whole list when it changed; null means empty. */
  starts?: StartJob[] | null;
}

export type AckErrorCode =
  | "agent_blocked"
  | "question_changed"
  | "still_blocked"
  | "stopped"
  | "read_only"
  | "forbidden"
  | "not_found"
  | "herdr_error"
  | "invalid_request"
  | "bad_endpoint"
  | "bad_settings"
  | "push_failed"
  | "too_many"
  | "queue_full"
  | "queue_failed"
  | "not_waiting"
  | "unknown_kind"
  | "invalid_name"
  | "name_taken"
  | "too_long";

export interface AckError {
  code: AckErrorCode;
  message: string;
}

/** push.subscribe and push.get acks carry the stored settings; a queue.add ack, the new item's id. */
export type Ack =
  | { type: "ack"; id: string; ok: true; settings?: PushSettings; item_id?: string }
  | { type: "ack"; id: string; ok: false; error: AckError };

export type StepKind = "you" | "ok" | "fail" | "now";

export interface Step {
  kind: StepKind;
  text: string;
  meta: string;
}

export interface QuestionOption {
  key: string; // the digit sent to the agent
  label: string;
  sub: string;
  primary: boolean;
  needs_note: boolean;
  /** AskUserQuestion's own-answer row: the note is the answer. */
  own: boolean;
}

/** One question of a multi-question ask, from its tab row; done once answered. */
export interface QuestionStep {
  header: string;
  done: boolean;
}

export interface ReviewItem {
  question: string;
  answer: string;
}

export interface Question {
  id: string;
  kind: "permission" | "ask" | "unknown";
  title: string;
  where: string | null;
  command: string | null;
  led_here: string | null;
  options: QuestionOption[];
  screen?: string; // only when kind is unknown
  /** An AskUserQuestion's tabs (one for a single question). */
  steps?: QuestionStep[];
  /** Set on the review step after the last of several questions. */
  review?: ReviewItem[];
}

export interface Detail {
  type: "detail";
  pane_id: string;
  source: "transcript" | "terminal";
  agent_label: string;
  status_since: string; // RFC3339
  now: string;
  /** The agent's whole latest message (trimmed, at most 8,000 characters), or the screen's last lines for a terminal agent. */
  last_message: string | null;
  /** The whole message, before the bound, ends in a question; always false for a terminal agent. */
  asks: boolean;
  steps: Step[];
  context_pct: number | null;
  pr: string | null;
  question: Question | null;
}

export interface Output {
  type: "output";
  pane_id: string;
  text: string;
}

export type ServerMessage = Snapshot | Patch | Ack | Detail | Output;

// app → server

/** No id and no ack. "terminal" says whether the agent view shows the pane's terminal, so the server polls its screen. */
export type Watch =
  | { type: "watch"; pane_id: string }
  | { type: "unwatch"; pane_id: string }
  | { type: "terminal"; pane_id: string; shown: boolean };

/** Requests that act on an agent. */
export type PaneRequest =
  | { id: string; type: "prompt"; pane_id: string; text: string }
  | { id: string; type: "keys"; pane_id: string; keys: string[] }
  | { id: string; type: "text"; pane_id: string; text: string }
  | { id: string; type: "answer"; pane_id: string; question_id: string; option: string; note?: string }
  | { id: string; type: "rename"; pane_id: string; name: string }
  | { id: string; type: "focus_on_desktop"; pane_id: string };

/** This device's push subscription; readers may send these too. push.test ignores quiet hours and suppression. */
export type PushRequest =
  | { id: string; type: "push.subscribe"; subscription: WebPushSubscription; settings: PushSettings }
  | { id: string; type: "push.get"; endpoint: string }
  | { id: string; type: "push.unsubscribe"; endpoint: string }
  | { id: string; type: "push.test"; endpoint: string };

/** Controllers only. queue.remove takes only a waiting item. */
export type QueueRequest =
  | { id: string; type: "queue.add"; target: QueueTarget; text: string }
  | { id: string; type: "queue.remove"; item_id: string };

/**
 * Controllers only: open a tab in the workspace, start the agent there and send
 * it the task. job_id is the app's, resent unchanged on a retry, so a resend
 * never opens a second tab; progress comes in the snapshot's starts.
 */
export interface StartRequest {
  id: string;
  type: "start";
  job_id: string;
  workspace_id: string;
  kind: string;
  name: string;
  task: string;
}

export type ClientRequest = PaneRequest | PushRequest | QueueRequest | StartRequest;

type DistributiveOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never;

/** A request before the socket gives it an id. */
export type RequestBody = DistributiveOmit<ClientRequest, "id">;

export type ClientMessage = Watch | ClientRequest;
