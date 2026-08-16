/**
 * cattery: publish this opencode session's state where cattery can read it.
 *
 * The contract is the same set of AGENT_* variables pi, Claude Code and Codex
 * publish, and the picker, the tab marker and the notifications treat all four
 * alike:
 *
 *   AGENT_KIND        "opencode"                                 (set once)
 *   AGENT_STATE       "working" | "blocked" | "idle"             (live)
 *   AGENT_MSG         most recent user message                   (live)
 *   AGENT_TOOL        the tool running now, "bash: go test ./..." (live)
 *   AGENT_TOOL_SINCE  unix seconds when that tool started         (live)
 *   AGENT_RESUME      the command that brings this session back  (per session)
 *
 * Unlike the pi extension, this plugin writes none of that itself: it runs
 * `cattery state <word> --kind opencode` with a JSON payload on stdin, once per
 * transition. Two reasons. A server plugin runs in a worker thread of the TUI
 * process, so `process.stdout` is the TUI's and cannot carry the OSC escape a
 * kitty window needs. And the Go writer already owns the /dev/tty -> `kitten @`
 * fallback, the batch ordering, the tmux ";" escape, the control-character
 * strip and the @AGENT_SINCE / @AGENT_WORKED / @AGENT_SEEN pane bookkeeping.
 *
 * Lifecycle mapping:
 *   plugin load                 -> state=idle, source=startup, tool cleared,
 *                                  which drops what a killed agent left here
 *   session.created (top level) -> the same, now naming the session
 *   chat.message                -> AGENT_MSG, AGENT_RESUME, state=working
 *   session.status busy         -> state=working
 *   session.status idle         -> tool cleared, state=idle
 *   session.status retry        -> nothing; the agent is still working
 *   permission/question asked   -> state=blocked
 *   permission/question replied -> state=working once none is outstanding
 *   tool.execute.before/after   -> AGENT_TOOL_SINCE, AGENT_TOOL
 *   dispose                     -> state=clear
 *
 * Outside both kitty and tmux the plugin registers nothing, so `opencode serve`
 * and `opencode --attach` cost nothing.
 */

import { spawnSync } from "node:child_process";

import type { Plugin } from "@opencode-ai/plugin";

/** Runs the cattery binary with a JSON payload on stdin, ignoring its output. */
type Runner = (file: string, args: string[], input: string) => void;

let runner: Runner = (file, args, input) => {
  spawnSync(file, args, { input, stdio: ["pipe", "ignore", "ignore"] });
};

/**
 * Replace the process runner. A test seam: it lets the published batches be
 * asserted without a cattery binary or a kitty window.
 */
export function setRunner(next: Runner): void {
  runner = next;
}

// Where the binary is. The managed kitty.conf block exports CATTERY_BIN, which
// is how a window started from the Dock finds a Homebrew install: kitty's own
// PATH there is launchd's. A plugin is a child of the opencode process, which a
// shell started, so the bare name works whenever that export is missing.
function catteryBin(): string {
  return process.env.CATTERY_BIN || "cattery";
}

function inKitty(): boolean {
  return typeof process.env.KITTY_WINDOW_ID === "string" && process.env.KITTY_WINDOW_ID.length > 0;
}

// A pane needs both halves: TMUX names the server, TMUX_PANE the pane to write
// to. Half the pair cannot address anything.
function inTmux(): boolean {
  return (
    typeof process.env.TMUX === "string" &&
    process.env.TMUX.length > 0 &&
    typeof process.env.TMUX_PANE === "string" &&
    process.env.TMUX_PANE.length > 0
  );
}

/** Where this agent can publish. */
function published(): boolean {
  return inTmux() || inKitty();
}

/** The stdin payload `cattery state` reads. Every field is optional. */
type Payload = {
  session_id?: string;
  prompt?: string;
  source?: string;
  /** "" deletes the tool pair; absent leaves it standing. */
  tool?: string;
  tool_since?: number;
};

type AgentState = "working" | "blocked" | "idle";

// Collapse a prompt to one trimmed line and cap its length. The Go writer caps
// it again at the same width; doing it here keeps the payload small.
const MAX_PROMPT = 200;

function sanitizeMessage(text: string): string {
  const oneLine = text.replace(/\s+/g, " ").trim();
  return oneLine.length > MAX_PROMPT ? oneLine.slice(0, MAX_PROMPT - 1) + "…" : oneLine;
}

// --- the running tool ---------------------------------------------------------

/** One tool call opencode has started and not finished. */
type ToolCall = { label: string; startedAt: number };

// How long a tool has to run before the host hears about it. `runner` is
// spawnSync, so a turn of 200 fast calls would otherwise cost 200 blocking
// forks. A tool worth showing runs for minutes, so the delay costs nothing that
// matters.
const TOOL_DEBOUNCE_MS = 2000;

// The one argument worth showing per built-in tool, read out of
// packages/opencode/src/tool/ at 1.18.18. Anything else, an MCP tool or a
// plugin's own included, publishes its bare name.
//
// The shell tool answers to "bash": its id and its permission key both stay
// that word for compatibility with saved permissions, and opencode's own source
// says the rename waits for 2.0. Nothing here breaks when it lands, the label
// just loses its command.
const TOOL_ARG: Record<string, string> = {
  bash: "command",
  read: "filePath",
  write: "filePath",
  edit: "filePath",
  lsp: "filePath",
  glob: "pattern",
  grep: "pattern",
  webfetch: "url",
  websearch: "query",
  skill: "name",
  task: "description",
};

// Shorter than the 200 AGENT_MSG uses: the label shares the picker's second
// line with the agent's directory and an elapsed time, and it is cut there
// rather than wrapped. Keep it in step with toolLimit in internal/state.
const MAX_TOOL_LABEL = 120;

// "bash: go test ./...", or the bare tool name when there is no argument to
// show. The name always leads, so a value can never start with the argument and
// be read as a flag by whatever publishes it.
function toolLabel(toolName: string, args: unknown): string {
  const key = TOOL_ARG[toolName];
  let detail = "";
  if (key !== undefined && typeof args === "object" && args !== null) {
    const value = (args as Record<string, unknown>)[key];
    if (typeof value === "string") detail = value.replace(/\s+/g, " ").trim();
  }
  const label = detail === "" ? toolName : `${toolName}: ${detail}`;
  return label.length > MAX_TOOL_LABEL ? label.slice(0, MAX_TOOL_LABEL - 1) + "…" : label;
}

// --- the events this plugin reads ---------------------------------------------

/**
 * The shape the event hook really receives: `{ id, type, properties }`, where
 * properties is the event's own data.
 *
 * Typed here rather than against the SDK's `Event` union, which is a stale
 * artifact: it carries permission.v2.* and question.v2.* events that only
 * packages/core publishes, and the session path this plugin watches does not
 * use those.
 */
type CatteryEvent = { type: string; properties?: Record<string, unknown> };

function str(properties: Record<string, unknown> | undefined, key: string): string {
  const value = properties?.[key];
  return typeof value === "string" ? value : "";
}

/** A "permission.asked" or "question.asked" carries the id, its answer names it. */
function requestID(event: CatteryEvent): string {
  return str(event.properties, "requestID") || str(event.properties, "id");
}

const server: Plugin = async ({ client }) => {
  if (!published()) return {};

  // --- what this window is showing --------------------------------------------

  let lastState: AgentState | null = null;

  // The session this window belongs to, and what is known about every session id
  // seen so far: true for a child. The task tool prompts a subagent through the
  // same entrypoint, so chat.message, session.status and the tool hooks all fire
  // with the child's id, and a child going idle inside the parent's turn would
  // draw the green "done" marker and fire a "finished" banner over an agent that
  // is still working.
  let primary: string | null = null;
  const isChild = new Map<string, boolean>();
  const lookups = new Map<string, Promise<boolean>>();

  // The permission and question requests waiting for an answer, by request id.
  // A set rather than a counter: opencode publishes a permission.replied for
  // every request a single "always" answer cascades onto, each with its own id,
  // so counting replies would take the agent out of "blocked" early and a
  // duplicate would take it below zero.
  const pending = new Set<string>();

  // The tool calls in flight, keyed by callID. opencode runs siblings
  // concurrently, so several are open at once.
  const openTools = new Map<string, ToolCall>();
  // What the host currently shows, so a repeat costs nothing. Both halves have
  // to match: two concurrent calls can share a label, and promoting the second
  // one has to move the timestamp.
  let publishedTool: ToolCall | null = null;
  let toolTimer: ReturnType<typeof setTimeout> | null = null;

  // --- publishing --------------------------------------------------------------

  // The session id rides on every batch, the way Claude's and Codex's hooks
  // carry theirs: the Go writer turns it into AGENT_RESUME, composing
  // "<prefix> --session <id>" and quoting the id itself.
  //
  // Nothing here may throw. opencode calls the event hook as
  // `void hook.event(...)` with no catch of its own, so a rejection from one of
  // these would be an unhandled rejection on the TUI's own worker, and a tab
  // marker must not be able to take the agent down. The next transition
  // publishes again anyway.
  function publish(state: AgentState | "clear", extra: Payload = {}): void {
    const payload: Payload = { ...extra };
    if (primary !== null) payload.session_id = primary;
    try {
      runner(catteryBin(), ["state", state, "--kind", "opencode"], JSON.stringify(payload));
    } catch {
      // Nothing to fall back to: the binary is the only way out of this process.
    }
  }

  /** Publish a state change, dropping a repeat of the same word. */
  function emit(state: AgentState): void {
    if (state === lastState) return;
    lastState = state;
    publish(state);
  }

  // --- the primary session -----------------------------------------------------

  /**
   * Whether this session is the one the window belongs to.
   *
   * `session.created` answers for every session opencode opens in this process,
   * which is the usual path. An id that arrives without one is the
   * `opencode --session <id>` resume case, and the server is asked once.
   */
  async function isPrimary(sessionID: string): Promise<boolean> {
    if (sessionID === "") return false;
    if (sessionID === primary) return true;
    const child = isChild.get(sessionID) ?? (await resolveChild(sessionID));
    if (child) return false;
    if (primary === null) primary = sessionID;
    return primary === sessionID;
  }

  async function resolveChild(sessionID: string): Promise<boolean> {
    let inflight = lookups.get(sessionID);
    if (inflight === undefined) {
      inflight = lookupChild(sessionID);
      lookups.set(sessionID, inflight);
    }
    const child = await inflight;
    lookups.delete(sessionID);
    isChild.set(sessionID, child);
    return child;
  }

  async function lookupChild(sessionID: string): Promise<boolean> {
    try {
      const answer = await client.session.get({ path: { id: sessionID } });
      const parentID = (answer as { data?: { parentID?: unknown } } | undefined)?.data?.parentID;
      return typeof parentID === "string" && parentID !== "";
    } catch {
      // The server could not say. Reading it as top-level is the cheaper wrong
      // answer: it costs one early "done" marker, where the other one would keep
      // a real agent out of the picker for its whole life.
      return false;
    }
  }

  // The batch that opens a session. "startup" is what makes the Go writer drop
  // AGENT_MSG and @AGENT_WORKED, so a window whose last agent was killed does
  // not open wearing that agent's prompt and reading as "done".
  function startSession(): void {
    pending.clear();
    resetTools();
    lastState = "idle";
    publish("idle", { source: "startup", tool: "" });
  }

  // --- the running tool --------------------------------------------------------

  // The earliest-started open call, never the newest. That call is the stall
  // candidate: a fast `read` starting beside a `bash` hung for 19 minutes would
  // otherwise restamp the timestamp and the stall would never fire. Map
  // iteration is insertion-ordered, so two calls stamped in the same second keep
  // the order opencode started them in.
  function earliestTool(): ToolCall | null {
    let best: ToolCall | null = null;
    for (const call of openTools.values()) {
      if (best === null || call.startedAt < best.startedAt) best = call;
    }
    return best;
  }

  // Write the earliest open call, or delete both variables when there is none.
  // The state word rides along because `cattery state` needs one; the pair is
  // what this batch is for.
  function publishTool(): void {
    const next = earliestTool();
    if (next === null) {
      if (publishedTool === null) return;
      clearTool();
      return;
    }
    if (publishedTool !== null && publishedTool.label === next.label && publishedTool.startedAt === next.startedAt) {
      return;
    }
    publishedTool = next;
    publish(lastState ?? "working", { tool: next.label, tool_since: next.startedAt });
  }

  function scheduleToolPublish(): void {
    if (toolTimer !== null) return;
    const timer = setTimeout(() => {
      toolTimer = null;
      // The only publish outside a hook, so the only one opencode's own dispatch
      // does not catch for us. A throw here would be an uncaught exception on
      // the event loop.
      try {
        publishTool();
      } catch {
        // Nothing to fall back to: the label is cosmetic and the next tool
        // boundary tries again.
      }
    }, TOOL_DEBOUNCE_MS);
    // A pending label must never hold the process open on its way out.
    timer.unref?.();
    toolTimer = timer;
  }

  function cancelToolPublish(): void {
    if (toolTimer === null) return;
    clearTimeout(toolTimer);
    toolTimer = null;
  }

  /** Delete both variables, whatever this process has published. */
  function clearTool(): void {
    publishedTool = null;
    publish(lastState ?? "idle", { tool: "" });
  }

  // Forget every open call and take the label off the host. Called at every turn
  // boundary: an interrupt tears the turn down with no tool.execute.after, and a
  // label pinned with an hours-old timestamp reads as stalled from the next
  // turn's first second.
  function resetTools(): void {
    openTools.clear();
    cancelToolPublish();
    publishedTool = null;
  }

  // --- events ------------------------------------------------------------------

  // session.created and session.updated are the only things that carry a
  // parentID. The hook inputs do not, which is why every id has to be learnt
  // here or asked for.
  async function onSessionLifecycle(event: CatteryEvent): Promise<void> {
    const sessionID = str(event.properties, "sessionID");
    const info = event.properties?.["info"];
    if (sessionID === "" || typeof info !== "object" || info === null) return;
    const parentID = str(info as Record<string, unknown>, "parentID");
    isChild.set(sessionID, parentID !== "");
    if (parentID !== "") return;
    // A top-level session opening: the first one, or the one /new just made.
    // Repointing here is what moves AGENT_RESUME off the session the user left.
    // Only session.created, because session.updated fires all through a turn.
    if (event.type !== "session.created" || sessionID === primary) return;
    primary = sessionID;
    startSession();
  }

  async function onSessionStatus(event: CatteryEvent): Promise<void> {
    const status = event.properties?.["status"];
    const type = typeof status === "object" && status !== null ? str(status as Record<string, unknown>, "type") : "";
    // A retry is opencode waiting on a provider, not the turn ending.
    if (type !== "busy" && type !== "idle") return;
    if (!(await isPrimary(str(event.properties, "sessionID")))) return;
    if (type === "busy") {
      emit("working");
      return;
    }
    if (lastState === "idle") return;
    pending.clear();
    resetTools();
    lastState = "idle";
    publish("idle", { tool: "" });
  }

  async function onAsked(event: CatteryEvent): Promise<void> {
    if (!(await isPrimary(str(event.properties, "sessionID")))) return;
    const id = requestID(event);
    if (id === "") return;
    pending.add(id);
    emit("blocked");
  }

  // Out of "blocked" only. A reply cannot reach an agent that is not waiting,
  // but publishing "working" over an idle one would put a running marker on a
  // tab whose turn has ended.
  async function onReplied(event: CatteryEvent): Promise<void> {
    if (!(await isPrimary(str(event.properties, "sessionID")))) return;
    pending.delete(requestID(event));
    if (pending.size > 0 || lastState !== "blocked") return;
    emit("working");
  }

  // Before any session event arrives, so a window whose agent was killed stops
  // showing that agent's marker as soon as this one loads. It names no session
  // yet, which leaves AGENT_RESUME pointing at the last one until session.created
  // or the first prompt repoints it.
  startSession();

  return {
    async event({ event }) {
      const e = event as unknown as CatteryEvent;
      switch (e.type) {
        case "session.created":
        case "session.updated":
          await onSessionLifecycle(e);
          return;
        case "session.status":
          await onSessionStatus(e);
          return;
        case "permission.asked":
        case "question.asked":
          await onAsked(e);
          return;
        case "permission.replied":
        case "question.replied":
        case "question.rejected":
          await onReplied(e);
          return;
        default:
          return;
      }
    },

    // Every prompt overwrites the last. The picker draws this beside a live
    // spinner and has to show the current request; the row already names the cwd
    // and the branch.
    async "chat.message"(input, output) {
      if (!(await isPrimary(input.sessionID))) return;
      const text = output.parts
        .filter((part) => part.type === "text")
        .map((part) => (part as { text?: string }).text ?? "")
        .join(" ");
      const prompt = sanitizeMessage(text);
      pending.clear();
      resetTools();
      lastState = "working";
      publish("working", { prompt, tool: "" });
    },

    async "tool.execute.before"(input, output) {
      if (!(await isPrimary(input.sessionID))) return;
      openTools.set(input.callID, {
        label: toolLabel(input.tool, output.args),
        startedAt: Math.floor(Date.now() / 1000),
      });
      scheduleToolPublish();
    },

    // No primary check: a call this window never opened is not in the map, and
    // the delete says so without a session lookup.
    async "tool.execute.after"(input) {
      if (!openTools.delete(input.callID)) return;
      if (openTools.size === 0 && publishedTool === null) {
        // Nothing reached the host and nothing is running: drop the pending
        // write rather than fork for a tool that is already over.
        cancelToolPublish();
        return;
      }
      scheduleToolPublish();
    },

    // The TUI's ctrl-c is a keybind rather than a signal, so a normal quit runs
    // this. An external kill does not, which is what the shell wrapper's
    // `cattery state clear` after the agent exits covers.
    async dispose() {
      cancelToolPublish();
      publish("clear");
    },
  };
};

export default { id: "cattery", server };
