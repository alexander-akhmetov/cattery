/**
 * cattery: publish per-window agent state as kitty user variables.
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
 *                                                 AGENT_STATE=idle,
 *                                                 AGENT_MSG cleared
 *   before_agent_start                         -> AGENT_MSG=<prompt>
 *   agent_start                                -> AGENT_STATE=working
 *   tool_execution_start (interactive tool)    -> AGENT_STATE=blocked
 *   tool_execution_end   (interactive tool)    -> AGENT_STATE=working
 *   agent_settled                              -> AGENT_STATE=idle
 *   session_shutdown                           -> AGENT_STATE, AGENT_MSG cleared
 *
 * Shutdown leaves AGENT_RESUME alone, because `cattery save` reads it off the
 * window long after the agent is gone.
 *
 * Idle comes from agent_settled rather than agent_end. pi can auto-retry,
 * auto-compact, or pick up a queued message after a run ends, and each would
 * flash "finished" and fire a notification mid-task.
 *
 * kitty strips the OSC sequence from the visible output, so emitting it is safe
 * while pi-tui owns the screen. Outside kitty the extension does nothing.
 *
 * Known limitation: pi's command-approval prompt fires no tool_execution_start
 * event, so the tab stays "working" while pi waits for the user.
 */

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

// Tool names that put pi into a "user must respond" state. Keep the set small.
// Add a tool only after checking that it blocks until the user answers.
const INTERACTIVE_TOOLS = new Set<string>(["ask_user_question"]);

function inKitty(): boolean {
  return typeof process.env.KITTY_WINDOW_ID === "string" && process.env.KITTY_WINDOW_ID.length > 0;
}

// Emit an OSC 1337 SetUserVar to the controlling terminal.
//
// A null `value` omits the `=value` part, which deletes the variable.
function setUserVar(key: string, value: string | null): void {
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
  if (!inKitty()) return;
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
  if (!inKitty()) return;

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
    if (lastState !== null) {
      lastState = null;
      setUserVar("AGENT_STATE", null);
    }
    setUserVar("AGENT_MSG", null);
  });
}
