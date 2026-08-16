/**
 * cattery: publish this agent's state where cattery can read it.
 *
 * In a kitty window that is a set of kitty user variables, and the cattery
 * watcher turns them into a tab marker. In a tmux pane it is the same contract
 * written as pane options, one "@" in front of each name, which the picker
 * reads directly. A pane wins whenever there is one: an agent in a detached
 * pane has no terminal for the escape to reach, and the tmux server can hand it
 * a KITTY_WINDOW_ID belonging to an unrelated window.
 *
 * The contract with the cattery kitty watcher:
 *
 *   AGENT_KIND        "pi"                                       (set once)
 *   AGENT_STATE       "working" | "blocked" | "idle"             (live)
 *   AGENT_MSG         most recent user message                   (live)
 *   AGENT_TOOL        the tool running now, "bash: go test ./..." (live)
 *   AGENT_TOOL_SINCE  unix seconds when that tool started         (live)
 *   AGENT_RESUME      the command that brings this session back  (per session)
 *
 * Each value is base64 inside OSC `1337;SetUserVar=KEY=...`. The watcher derives
 * AGENT_DISPLAY from AGENT_STATE and its own seen/unseen bookkeeping, and owns
 * the tab marker, the notifications, and the OS-window title. The picker draws
 * AGENT_MSG as the row's current-task line, and AGENT_TOOL with a live elapsed
 * time in front of it.
 *
 * Lifecycle mapping:
 *   session_start                              -> AGENT_KIND=pi, AGENT_RESUME,
 *                                                 AGENT_MSG cleared,
 *                                                 AGENT_TOOL* cleared,
 *                                                 AGENT_STATE=idle,
 *                                                 @AGENT_WORKED cleared (tmux)
 *   before_agent_start                         -> AGENT_MSG=<prompt>
 *   agent_start                                -> AGENT_TOOL* cleared,
 *                                                 AGENT_STATE=working
 *   tool_execution_start                       -> AGENT_TOOL_SINCE, AGENT_TOOL
 *   tool_execution_start (interactive tool)    -> AGENT_STATE=blocked
 *   tool_execution_end                         -> the next-earliest call, or
 *                                                 AGENT_TOOL* cleared
 *   tool_execution_end   (interactive tool)    -> AGENT_STATE=working
 *   agent_settled                              -> AGENT_TOOL* cleared,
 *                                                 AGENT_STATE=idle
 *   session_shutdown                           -> AGENT_MSG, AGENT_TOOL*,
 *                                                 AGENT_STATE cleared
 *
 * AGENT_STATE is written last in every one of those, because writing it is what
 * wakes the watcher: a variable written after it is missing from the transition
 * the watcher publishes. AGENT_TOOL_SINCE goes before AGENT_TOOL for the same
 * reason one level down: AGENT_TOOL is the key the watcher reacts to, so the
 * other order would have it read the previous tool's timestamp.
 *
 * Shutdown leaves AGENT_RESUME alone, because `cattery save` reads it off the
 * window long after the agent is gone.
 *
 * Idle comes from agent_settled rather than agent_end. pi can auto-retry,
 * auto-compact, or pick up a queued message after a run ends, and each would
 * flash "finished" and fire a notification mid-task.
 *
 * kitty strips the OSC sequence from the visible output, so emitting it is safe
 * while pi-tui owns the screen. Outside both kitty and tmux the extension does
 * nothing.
 *
 * Known limitation: pi's command-approval prompt is not a state of its own, so
 * the tab stays "working" while pi waits for the user. tool_execution_start
 * fires before prepareToolCall, which is where the approval gate runs, so a
 * tool waiting for an answer does publish AGENT_TOOL and does age towards
 * "stalled".
 */

import { spawnSync } from "node:child_process";

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

// Tool names that put pi into a "user must respond" state. Keep the set small.
// Add a tool only after checking that it blocks until the user answers.
const INTERACTIVE_TOOLS = new Set<string>(["ask_user_question"]);

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

// Where this agent can publish.
function published(): boolean {
  return inTmux() || inKitty();
}

/** Runs a command to completion, ignoring its output. */
type Runner = (file: string, args: string[]) => void;

let runner: Runner = (file, args) => {
  spawnSync(file, args, { stdio: "ignore" });
};

/**
 * Replace the process runner. A test seam: it lets the tmux path be asserted
 * without a tmux server.
 */
export function setRunner(next: Runner): void {
  runner = next;
}

/** One variable update. A null value deletes the variable. */
type VarUpdate = [key: string, value: string | null];

// C0 and C1 control characters. No published value may carry one.
//
// \x1f separates the fields of a `tmux list-panes` row, and the picker drops a
// row whose field count is wrong, so one of these in any value takes the agent
// out of the picker with no error anywhere. On kitty the value travels base64
// and reaches the picker instead, which draws it into its own frame: one \x1b
// there moves the cursor rather than a column. JavaScript's \s matches neither
// byte, so sanitizeMessage's whitespace fold does not cover this.
const CONTROL_CHARS = /[\x00-\x1f\x7f-\x9f]/g;

// Publish several user variables at once, on whichever transport reaches this
// agent: one OSC run on kitty, one chained command on tmux. Batching is what
// makes a tool boundary one process rather than two.
//
// Every value is stripped here rather than at each call site, so nothing a
// prompt, a tool argument or a session path carries can hide the agent.
function setUserVars(updates: VarUpdate[]): void {
  const clean: VarUpdate[] = updates.map(([key, value]) => [
    key,
    value === null ? null : value.replace(CONTROL_CHARS, " "),
  ]);
  if (inTmux()) {
    // The pane wins whenever there is one, for the two reasons at the top of
    // this file.
    const pane: PaneUpdate[] = [];
    for (const [key, value] of clean) pane.push(...paneUpdates(key, value));
    runner("tmux", paneArgs(pane));
    return;
  }
  // base64 over the UTF-8 bytes. AGENT_MSG carries prompt text in any script,
  // and kitty decodes the value as UTF-8.
  let payload = "";
  for (const [key, value] of clean) {
    if (value === null) {
      payload += `\x1b]1337;SetUserVar=${key}\x07`;
      continue;
    }
    const b64 = Buffer.from(value, "utf-8").toString("base64");
    payload += `\x1b]1337;SetUserVar=${key}=${b64}\x07`;
  }
  if (payload === "") return;
  // Write directly, around pi-tui's stdout pipeline. The OSC sequence is
  // invisible in the rendered TUI, but the pi.ui APIs would log it as content.
  process.stdout.write(payload);
}

/** Publish one user variable. A null `value` deletes it. */
function setUserVar(key: string, value: string | null): void {
  setUserVars([[key, value]]);
}

/** One pane-option update. A null value deletes the option. */
type PaneUpdate = [key: string, value: string | null];

// Nothing derives a display state for a tmux pane the way the kitty watcher
// does for a window, so the picker reads it off these options. This mirrors
// internal/state/tmux.go: the two writers have to leave a pane in the same
// shape.
function paneUpdates(key: string, value: string | null): PaneUpdate[] {
  const updates: PaneUpdate[] = [[key, value]];
  if (key !== "AGENT_STATE") return updates;
  if (value === null) {
    // The agent is gone. Left standing, "has worked" would make the next agent
    // in this pane report "done" the moment it starts idle.
    updates.push(["AGENT_WORKED", null]);
    return updates;
  }
  // This runs only on a state change: emit() drops a repeat, so the picker's
  // elapsed time counts one state rather than one event.
  updates.push(["AGENT_SINCE", String(Math.floor(Date.now() / 1000))]);
  if (value === "working" || value === "blocked") {
    updates.push(["AGENT_WORKED", "1"], ["AGENT_SEEN", null]);
  }
  return updates;
}

// Chain the updates into one tmux command line. A ";" is tmux's command
// separator, so a state change costs one process rather than four.
function paneArgs(updates: PaneUpdate[]): string[] {
  const pane = process.env.TMUX_PANE ?? "";
  const args: string[] = [];
  for (const [key, value] of updates) {
    if (args.length > 0) args.push(";");
    args.push("set", "-p");
    if (value === null) args.push("-u");
    args.push("-t", pane, `@${key}`);
    if (value !== null) args.push(escapeArg(value));
  }
  return args;
}

// Protect a value from the command splitter. tmux ends a command at any
// argument ending in ";", and AGENT_MSG carries whatever the user typed, so an
// unescaped prompt loses its trailing ";" and a prompt that is only ";" also
// drops the updates chained behind it. "\;" is the escaped form tmux hands back
// as ";". This mirrors escapeArg in internal/state/tmux.go.
function escapeArg(value: string): string {
  if (!value.endsWith(";")) return value;
  return `${value.slice(0, -1)}\\;`;
}

// Forget what a previous agent in this pane finished.
//
// A pane outlives its agents, and only a clean session_shutdown clears
// @AGENT_WORKED. After a pi that was killed the option is still set, and the
// picker reads this session's opening idle as "done" before it has done
// anything. Nothing to do in kitty: the watcher keeps that bookkeeping per
// window and drops it when the window closes.
function clearPaneWork(): void {
  if (!inTmux()) return;
  runner("tmux", paneArgs([["AGENT_WORKED", null]]));
}

type AgentState = "working" | "blocked" | "idle";

let lastState: AgentState | null = null;
let blockedDepth = 0;

// Collapse a prompt to one trimmed line and cap its length. The picker draws it
// on one row, and the value travels inside an OSC escape.
function sanitizeMessage(text: string): string {
  const oneLine = text.replace(/\s+/g, " ").trim();
  const max = 200;
  return oneLine.length > max ? oneLine.slice(0, max - 1) + "\u2026" : oneLine;
}

// --- the running tool ---------------------------------------------------------

/** One tool call pi has started and not finished. */
type ToolCall = { label: string; startedAt: number };

// The calls in flight, keyed by toolCallId. pi runs siblings concurrently, so
// several are open at once and an immediate failure can end before a sibling
// starts.
const openTools = new Map<string, ToolCall>();

// What the host currently shows, so a repeat costs nothing. Both halves have to
// match: two concurrent calls can share a label, and promoting the second one
// has to move the timestamp.
let publishedTool: ToolCall | null = null;

let toolTimer: ReturnType<typeof setTimeout> | null = null;

// How long a tool has to run before the host hears about it. `runner` is
// spawnSync on pi's main thread, and a turn of 200 fast calls would otherwise
// cost 400 blocking forks where a whole turn costs about four today. A tool
// worth showing runs for minutes, so the delay costs nothing that matters.
const TOOL_DEBOUNCE_MS = 2000;

// The one argument worth showing per built-in tool. Anything else, an extension
// tool included, publishes its bare name.
const TOOL_ARG: Record<string, string> = {
  bash: "command",
  read: "path",
  write: "path",
  edit: "path",
  ls: "path",
  grep: "pattern",
  find: "pattern",
};

// Shorter than the 200 AGENT_MSG uses: the label shares the picker's second
// line with the agent's directory and an elapsed time, and it is cut there
// rather than wrapped.
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
  return label.length > MAX_TOOL_LABEL ? label.slice(0, MAX_TOOL_LABEL - 1) + "\u2026" : label;
}

// The earliest-started open call, never the newest. That call is the stall
// candidate: a fast `read` starting beside a `bash` hung for 19 minutes would
// otherwise restamp the timestamp and the stall would never fire. Map iteration
// is insertion-ordered, so two calls stamped in the same second keep the order
// pi started them in.
function earliestTool(): ToolCall | null {
  let best: ToolCall | null = null;
  for (const call of openTools.values()) {
    if (best === null || call.startedAt < best.startedAt) best = call;
  }
  return best;
}

// Write the earliest open call, or delete both variables when there is none.
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
  setUserVars([
    ["AGENT_TOOL_SINCE", String(next.startedAt)],
    ["AGENT_TOOL", next.label],
  ]);
}

function scheduleToolPublish(): void {
  if (toolTimer !== null) return;
  const timer = setTimeout(() => {
    toolTimer = null;
    // The only publish outside a pi handler, so the only one pi's dispatch does
    // not catch for us. A throw here would be an uncaught exception on the
    // event loop and would take the agent down for a tab marker.
    try {
      publishTool();
    } catch {
      // Nothing to fall back to: the label is cosmetic and the next tool
      // boundary tries again.
    }
  }, TOOL_DEBOUNCE_MS);
  // A pending label must never hold pi open on its way out.
  timer.unref?.();
  toolTimer = timer;
}

function cancelToolPublish(): void {
  if (toolTimer === null) return;
  clearTimeout(toolTimer);
  toolTimer = null;
}

// Delete both variables, whatever this process has published. A window or a pane
// outlives its agents and nothing else clears the pair, so a session opening in
// one has to drop what a killed agent left behind.
function clearTool(): void {
  publishedTool = null;
  setUserVars([
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
  ]);
}

// Forget every open call and take the label off the host. Called at every run
// boundary, `agent_start` included: an interrupt tears the process down with no
// tool_execution_end, so the next run has to clear what the last one left. A
// label pinned with an hours-old timestamp reads as stalled from that run's
// first second.
function resetTools(): void {
  openTools.clear();
  cancelToolPublish();
  publishTool();
}

function emit(state: AgentState): void {
  if (!published()) return;
  if (state === lastState) return;
  lastState = state;
  setUserVar("AGENT_STATE", state);
}

// Characters that survive a POSIX shell unquoted. One other character sends the
// whole word through single quotes. This mirrors internal/shellquote in Go.
const SHELL_SAFE = /^[A-Za-z0-9@%+=:,./_-]+$/;

function shellQuote(value: string): string {
  if (SHELL_SAFE.test(value)) return value;
  // A single quote cannot appear inside single quotes, so close, escape it,
  // and reopen: the POSIX '\'' idiom.
  return `'${value.replaceAll("'", "'\\''")}'`;
}

// What the resume command starts with. The pi-only name wins over the shared
// one. cattery's Claude writer appends "--resume <id>" to the same prefix, so
// an exported CATTERY_RESUME_PREFIX="nono run claude" would make this session
// publish "nono run claude --session <pi transcript>".
//
// An empty value counts as unset, as it does in the Go writer. An
// exported-but-cleared variable would otherwise publish a command with no
// program in front of it.
function resumePrefix(): string {
  return process.env.CATTERY_RESUME_PREFIX_PI || process.env.CATTERY_RESUME_PREFIX || "pi";
}

// The command that brings this session back, or null when there is none.
// `cattery restore` types it at the prompt of the restored window.
//
// Only the session path is quoted. The prefix is a raw command fragment,
// because an override adds a wrapper of several words that the session cannot
// guess, such as a sandbox or a profile flag.
function resumeCommand(ctx: ExtensionContext): string | null {
  let sessionFile: string | undefined;
  try {
    sessionFile = ctx.sessionManager.getSessionFile();
  } catch {
    // Reading the session file must never break session_start. A pi without a
    // session manager loses the resume command and keeps its tab marker.
    return null;
  }
  if (sessionFile === undefined || sessionFile === "") return null;
  return `${resumePrefix()} --session ${shellQuote(sessionFile)}`;
}

export default function (pi: ExtensionAPI) {
  if (!published()) return;

  // session_start fires for startup, new, resume, fork, and reload, so /new and
  // /fork repoint AGENT_RESUME at the session actually in the window.
  pi.on("session_start", async (_event, ctx) => {
    // AGENT_KIND is the agent identity; never changes during a session.
    setUserVar("AGENT_KIND", "pi");
    // Publish the resume command, or clear one a prior agent left behind.
    setUserVar("AGENT_RESUME", resumeCommand(ctx));
    // Reset bookkeeping for the new session.
    blockedDepth = 0;
    lastState = null;
    clearPaneWork();
    // Drop a stale message and a stale tool from a prior agent that ran here.
    setUserVar("AGENT_MSG", null);
    openTools.clear();
    cancelToolPublish();
    clearTool();
    emit("idle");
  });

  // Every prompt overwrites the last. The picker draws this beside a live
  // spinner and has to show the current request; the row already names the cwd
  // and the branch.
  pi.on("before_agent_start", async (event) => {
    const msg = sanitizeMessage(event.prompt ?? "");
    if (msg === "") return;
    setUserVar("AGENT_MSG", msg);
  });

  pi.on("agent_start", async () => {
    blockedDepth = 0;
    resetTools();
    emit("working");
  });

  pi.on("agent_settled", async () => {
    blockedDepth = 0;
    resetTools();
    emit("idle");
  });

  pi.on("tool_execution_start", async (event) => {
    // An interactive tool publishes no label. It sets "blocked", which already
    // carries its own elapsed time through AGENT_SINCE, and a question the user
    // has not answered is not a stall.
    if (!INTERACTIVE_TOOLS.has(event.toolName)) {
      openTools.set(event.toolCallId, {
        label: toolLabel(event.toolName, event.args),
        startedAt: Math.floor(Date.now() / 1000),
      });
      scheduleToolPublish();
      return;
    }
    blockedDepth += 1;
    emit("blocked");
  });

  pi.on("tool_execution_end", async (event) => {
    if (!INTERACTIVE_TOOLS.has(event.toolName)) {
      if (!openTools.delete(event.toolCallId)) return;
      if (openTools.size === 0 && publishedTool === null) {
        // Nothing reached the host and nothing is running: drop the pending
        // write rather than fork for a tool that is already over.
        cancelToolPublish();
        return;
      }
      scheduleToolPublish();
      return;
    }
    blockedDepth = Math.max(0, blockedDepth - 1);
    if (blockedDepth === 0) emit("working");
  });

  pi.on("session_shutdown", async () => {
    // Clear AGENT_STATE so the tab bar drops the marker at once. Keep
    // AGENT_KIND so a quick `/resume` keeps its tag; the watcher tolerates a
    // kind with no state.
    //
    // AGENT_MSG and the tool go first: clearing AGENT_STATE is what wakes the
    // watcher, and the event it publishes would otherwise carry the last
    // prompt.
    setUserVar("AGENT_MSG", null);
    resetTools();
    if (lastState !== null) {
      lastState = null;
      setUserVar("AGENT_STATE", null);
    }
  });
}
