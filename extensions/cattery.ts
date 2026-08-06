/**
 * cattery: emit per-window agent state via kitty user variables.
 *
 * Cooperative contract with the cattery kitty watcher:
 *
 *   AGENT_KIND    "pi"                                       (constant, set once)
 *   AGENT_STATE   "working" | "blocked" | "idle"             (live)
 *   AGENT_MSG     most recent user message                   (live)
 *
 * All are written as base64-encoded values inside OSC `1337;SetUserVar=KEY=...`.
 * The kitty watcher derives AGENT_DISPLAY ("working"|"blocked"|"done"|"idle")
 * from AGENT_STATE plus its own seen/unseen bookkeeping. The watcher owns the
 * tab bar glyph, the desktop notifications, and the OS-window-title summary. The
 * cattery picker shows AGENT_MSG as the row's current-task line.
 *
 * Lifecycle mapping:
 *   session_start                              -> AGENT_KIND=pi, AGENT_STATE=idle,
 *                                                 AGENT_MSG cleared
 *   before_agent_start                         -> AGENT_MSG=<prompt>
 *   agent_start                                -> AGENT_STATE=working
 *   tool_execution_start (interactive tool)    -> AGENT_STATE=blocked
 *   tool_execution_end   (interactive tool)    -> AGENT_STATE=working
 *   agent_settled                              -> AGENT_STATE=idle
 *   session_shutdown                           -> AGENT_STATE, AGENT_MSG cleared
 *
 * Idle comes from agent_settled rather than agent_end: pi can auto-retry,
 * auto-compact, or pick up a queued message after a run ends, and each of
 * those would otherwise flash "finished" and fire a notification mid-task.
 *
 * kitty strips the OSC sequence from the visible terminal output, so emitting it
 * is safe even while pi-tui owns the screen. Does nothing outside kitty.
 *
 * Known limitation: pi's built-in command-approval prompt fires no
 * tool_execution_start event, so the tab stays "working" while pi waits for the
 * user. It can only show "blocked" once pi exposes a signal for that prompt.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Tool names that put pi into a "user must respond" state. Keep the set small:
// add a tool only after checking that it really blocks until the user answers.
const INTERACTIVE_TOOLS = new Set<string>(["ask_user_question"]);

function inKitty(): boolean {
  return typeof process.env.KITTY_WINDOW_ID === "string" && process.env.KITTY_WINDOW_ID.length > 0;
}

// Emit an OSC 1337 SetUserVar to the controlling terminal.
//
// Empty `value` (no `=value` part) deletes the variable on the window.
function setUserVar(key: string, value: string | null): void {
  // base64-encode the UTF-8 bytes: AGENT_MSG carries prompt text in any script,
  // and kitty decodes the value as UTF-8.
  let payload: string;
  if (value === null) {
    payload = `\x1b]1337;SetUserVar=${key}\x07`;
  } else {
    const b64 = Buffer.from(value, "utf-8").toString("base64");
    payload = `\x1b]1337;SetUserVar=${key}=${b64}\x07`;
  }
  // Bypass pi-tui's normal stdout pipeline by writing directly; OSC is
  // invisible to the rendered TUI but pi.ui APIs would log it as content.
  process.stdout.write(payload);
}

type AgentState = "working" | "blocked" | "idle";

let lastState: AgentState | null = null;
let blockedDepth = 0;

// Collapse a prompt to one trimmed line and cap its length: the picker draws it
// on a single row, and the value travels inside an OSC escape.
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

export default function (pi: ExtensionAPI) {
  if (!inKitty()) return;

  pi.on("session_start", async () => {
    // AGENT_KIND is the agent identity; never changes during a session.
    setUserVar("AGENT_KIND", "pi");
    // Reset bookkeeping for the new session.
    blockedDepth = 0;
    lastState = null;
    // Drop a stale message from a prior agent that ran in this window.
    setUserVar("AGENT_MSG", null);
    emit("idle");
  });

  // Every prompt, not only the first: the picker draws this beside a live
  // spinner, so in a long session the opening request is the wrong answer to
  // "what is this agent doing". The row already names the cwd and branch.
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
    // Clear AGENT_STATE so the tab bar drops the glyph immediately. Keep
    // AGENT_KIND so a quick `/resume` doesn't lose the kind tag (the watcher
    // tolerates a stale kind without state).
    if (lastState !== null) {
      lastState = null;
      setUserVar("AGENT_STATE", null);
    }
    setUserVar("AGENT_MSG", null);
  });
}
