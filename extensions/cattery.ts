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
 *   AGENT_KIND    "pi"                                       (set once)
 *   AGENT_STATE   "working" | "blocked" | "idle"             (live)
 *   AGENT_MSG     most recent user message                   (live)
 *   AGENT_RESUME  the command that brings this session back  (per session)
 *
 * Each value is base64 inside OSC `1337;SetUserVar=KEY=...`. The watcher derives
 * AGENT_DISPLAY from AGENT_STATE and its own seen/unseen bookkeeping, and owns
 * the tab marker, the notifications, and the OS-window title. The picker draws
 * AGENT_MSG as the row's current-task line.
 *
 * Lifecycle mapping:
 *   session_start                              -> AGENT_KIND=pi, AGENT_RESUME,
 *                                                 AGENT_MSG cleared,
 *                                                 AGENT_STATE=idle,
 *                                                 @AGENT_WORKED cleared (tmux)
 *   before_agent_start                         -> AGENT_MSG=<prompt>
 *   agent_start                                -> AGENT_STATE=working
 *   tool_execution_start (interactive tool)    -> AGENT_STATE=blocked
 *   tool_execution_end   (interactive tool)    -> AGENT_STATE=working
 *   agent_settled                              -> AGENT_STATE=idle
 *   session_shutdown                           -> AGENT_MSG, AGENT_STATE cleared
 *
 * AGENT_STATE is written last in every one of those, because writing it is what
 * wakes the watcher: a variable written after it is missing from the transition
 * the watcher publishes.
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
 * Known limitation: pi's command-approval prompt fires no tool_execution_start
 * event, so the tab stays "working" while pi waits for the user.
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

// Publish one user variable, on whichever transport reaches this agent.
//
// A null `value` deletes the variable.
function setUserVar(key: string, value: string | null): void {
  if (inTmux()) {
    // The pane wins whenever there is one, for the two reasons at the top of
    // this file.
    runner("tmux", paneArgs(paneUpdates(key, value)));
    return;
  }
  // base64 over the UTF-8 bytes. AGENT_MSG carries prompt text in any script,
  // and kitty decodes the value as UTF-8.
  let payload: string;
  if (value === null) {
    payload = `\x1b]1337;SetUserVar=${key}\x07`;
  } else {
    const b64 = Buffer.from(value, "utf-8").toString("base64");
    payload = `\x1b]1337;SetUserVar=${key}=${b64}\x07`;
  }
  // Write directly, around pi-tui's stdout pipeline. The OSC sequence is
  // invisible in the rendered TUI, but the pi.ui APIs would log it as content.
  process.stdout.write(payload);
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
    // Drop a stale message from a prior agent that ran in this window.
    setUserVar("AGENT_MSG", null);
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
    emit("working");
  });

  pi.on("agent_settled", async () => {
    blockedDepth = 0;
    emit("idle");
  });

  pi.on("tool_execution_start", async (event) => {
    if (!INTERACTIVE_TOOLS.has(event.toolName)) return;
    blockedDepth += 1;
    emit("blocked");
  });

  pi.on("tool_execution_end", async (event) => {
    if (!INTERACTIVE_TOOLS.has(event.toolName)) return;
    blockedDepth = Math.max(0, blockedDepth - 1);
    if (blockedDepth === 0) emit("working");
  });

  pi.on("session_shutdown", async () => {
    // Clear AGENT_STATE so the tab bar drops the marker at once. Keep
    // AGENT_KIND so a quick `/resume` keeps its tag; the watcher tolerates a
    // kind with no state.
    //
    // AGENT_MSG goes first: clearing AGENT_STATE is what wakes the watcher, and
    // the event it publishes would otherwise carry the last prompt.
    setUserVar("AGENT_MSG", null);
    if (lastState !== null) {
      lastState = null;
      setUserVar("AGENT_STATE", null);
    }
  });
}
