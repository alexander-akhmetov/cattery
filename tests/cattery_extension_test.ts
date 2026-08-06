/**
 * Unit tests for extensions/cattery.ts, the pi extension.
 *
 * The extension talks to kitty by writing an OSC escape to stdout, and learns
 * what the agent is doing from pi lifecycle events. Both ends are replaced
 * here. `FakePi` records the handlers the extension registers and fires them on
 * demand. `fire` swaps `process.stdout.write` for one event, so the escapes are
 * decoded instead of printed.
 *
 * Run with `make test-ts`, or `npm test`.
 */

import assert from "node:assert/strict";
import test from "node:test";

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

import register from "../extensions/cattery.ts";

/** One user variable write. A null value means the variable was deleted. */
type VarWrite = [key: string, value: string | null];

const SET_USER_VAR = /^\x1b\]1337;SetUserVar=([A-Z_]+)(?:=([A-Za-z0-9+/=]*))?\x07$/;

function parseSetUserVar(chunk: string): VarWrite {
  const match = SET_USER_VAR.exec(chunk);
  if (match === null) {
    // Anything else on stdout would appear in the middle of pi's TUI.
    throw new Error(`not a SetUserVar escape: ${JSON.stringify(chunk)}`);
  }
  const [, key, encoded] = match;
  return [key!, encoded === undefined ? null : Buffer.from(encoded, "base64").toString("utf-8")];
}

type Handler = (event: Record<string, unknown>, ctx: FakeContext) => Promise<unknown>;

/**
 * The half of pi's ExtensionContext the extension reads.
 *
 * `getSessionFile` returns undefined for an ephemeral session, as pi does under
 * `--no-session`.
 */
type FakeContext = { sessionManager: { getSessionFile(): string | undefined } };

function context(sessionFile: string | undefined): FakeContext {
  return { sessionManager: { getSessionFile: () => sessionFile } };
}

/** The session file every test uses unless it is testing the path itself. */
const SESSION_FILE = "/tmp/pi-session.jsonl";

class FakePi {
  readonly handlers = new Map<string, Handler>();

  /** Stands in for the context pi passes to every handler. */
  ctx: FakeContext = context(SESSION_FILE);

  on(event: string, handler: Handler): void {
    this.handlers.set(event, handler);
  }

  async emit(event: string, payload: Record<string, unknown>): Promise<void> {
    const handler = this.handlers.get(event);
    assert.ok(handler !== undefined, `the extension registered no handler for ${event}`);
    await handler({ type: event, ...payload }, this.ctx);
  }
}

/** Fire one pi event and return the user variables the extension wrote. */
async function fire(pi: FakePi, event: string, payload: Record<string, unknown> = {}): Promise<VarWrite[]> {
  const written: VarWrite[] = [];
  const original = process.stdout.write;
  process.stdout.write = ((chunk: string | Uint8Array): boolean => {
    written.push(parseSetUserVar(typeof chunk === "string" ? chunk : Buffer.from(chunk).toString("utf-8")));
    return true;
  }) as typeof process.stdout.write;
  try {
    await pi.emit(event, payload);
  } finally {
    process.stdout.write = original;
  }
  return written;
}

/**
 * A registered extension in a window that has already started its session.
 *
 * The extension keeps its state in module variables, so every test starts a
 * session to reset them, as pi does.
 */
async function startedSession(): Promise<FakePi> {
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  register(pi as unknown as ExtensionAPI);
  await fire(pi, "session_start");
  return pi;
}

test("outside kitty the extension registers nothing", () => {
  const saved = process.env.KITTY_WINDOW_ID;
  delete process.env.KITTY_WINDOW_ID;
  try {
    const pi = new FakePi();
    register(pi as unknown as ExtensionAPI);
    assert.equal(pi.handlers.size, 0);
  } finally {
    if (saved !== undefined) process.env.KITTY_WINDOW_ID = saved;
  }
});

test("a session opens as an idle pi agent with no stale message", async () => {
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  register(pi as unknown as ExtensionAPI);

  const written = await fire(pi, "session_start");

  assert.deepEqual(written, [
    ["AGENT_KIND", "pi"],
    ["AGENT_RESUME", `pi --session ${SESSION_FILE}`],
    // A previous agent in this window may have left one behind.
    ["AGENT_MSG", null],
    ["AGENT_STATE", "idle"],
  ]);
});

/** Fire session_start with a given session file and return only AGENT_RESUME. */
async function resumeOn(sessionFile: string | undefined): Promise<VarWrite[]> {
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  pi.ctx = context(sessionFile);
  register(pi as unknown as ExtensionAPI);
  const written = await fire(pi, "session_start");
  return written.filter(([key]) => key === "AGENT_RESUME");
}

test("session_start publishes the command that resumes this session", async () => {
  assert.deepEqual(await resumeOn(SESSION_FILE), [["AGENT_RESUME", "pi --session /tmp/pi-session.jsonl"]]);
});

/** Run resumeOn with the two prefix variables set, and put them back after. */
async function resumeWithPrefix(shared?: string, pi?: string): Promise<VarWrite[]> {
  const savedShared = process.env.CATTERY_RESUME_PREFIX;
  const savedPi = process.env.CATTERY_RESUME_PREFIX_PI;
  const set = (name: string, value: string | undefined) => {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  };
  set("CATTERY_RESUME_PREFIX", shared);
  set("CATTERY_RESUME_PREFIX_PI", pi);
  try {
    return await resumeOn(SESSION_FILE);
  } finally {
    set("CATTERY_RESUME_PREFIX", savedShared);
    set("CATTERY_RESUME_PREFIX_PI", savedPi);
  }
}

test("the resume prefix is overridable, for a sandbox or a wrapper", async () => {
  assert.deepEqual(await resumeWithPrefix("nono run pi"), [
    ["AGENT_RESUME", "nono run pi --session /tmp/pi-session.jsonl"],
  ]);
});

// cattery's Claude writer appends "--resume <id>" to the same prefix. Without a
// pi-only name, an exported CATTERY_RESUME_PREFIX="nono run claude" would make
// this session publish a Claude command aimed at a pi transcript, and
// `cattery restore -run` would press return on it.
test("the pi prefix wins over the shared one", async () => {
  assert.deepEqual(await resumeWithPrefix("nono run claude", "nono run pi"), [
    ["AGENT_RESUME", "nono run pi --session /tmp/pi-session.jsonl"],
  ]);
});

// The Go writer treats an empty value as unset, and so does this one. An
// exported-but-cleared variable would otherwise publish " --session <file>",
// with no program in front of it.
test("an empty prefix falls back to the default", async () => {
  assert.deepEqual(await resumeWithPrefix("", ""), [["AGENT_RESUME", "pi --session /tmp/pi-session.jsonl"]]);
});

test("a session path that needs quoting gets it", async () => {
  // pi derives the session directory from the cwd, and a cwd can hold spaces.
  // The iCloud path on this machine does. Restore types this at a shell prompt.
  assert.deepEqual(await resumeOn("/tmp/my sessions/a.jsonl"), [
    ["AGENT_RESUME", "pi --session '/tmp/my sessions/a.jsonl'"],
  ]);
});

test("an ephemeral session clears any resume command left in the window", async () => {
  // pi --no-session has no session file, and the window can still carry the
  // value a previous agent published.
  assert.deepEqual(await resumeOn(undefined), [["AGENT_RESUME", null]]);
});

test("the extension survives a pi with no session manager", async () => {
  // Reading the session file must never break session_start. Without the read
  // the window loses its resume command; with it the agent loses its marker.
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  pi.ctx = undefined as unknown as FakeContext;
  register(pi as unknown as ExtensionAPI);

  const written = await fire(pi, "session_start");

  assert.deepEqual(written, [
    ["AGENT_KIND", "pi"],
    ["AGENT_RESUME", null],
    ["AGENT_MSG", null],
    ["AGENT_STATE", "idle"],
  ]);
});

test("the run publishes the prompt before it publishes working", async () => {
  const pi = await startedSession();

  const message = await fire(pi, "before_agent_start", { prompt: "fix the tab bar" });
  const state = await fire(pi, "agent_start");

  assert.deepEqual(message, [["AGENT_MSG", "fix the tab bar"]]);
  assert.deepEqual(state, [["AGENT_STATE", "working"]]);
});

test("every prompt overwrites the last", async () => {
  const pi = await startedSession();
  await fire(pi, "before_agent_start", { prompt: "first" });
  await fire(pi, "agent_start");
  await fire(pi, "agent_settled");

  const written = await fire(pi, "before_agent_start", { prompt: "second" });

  assert.deepEqual(written, [["AGENT_MSG", "second"]]);
});

test("the prompt is collapsed to one line and capped", async () => {
  const cases: Array<[name: string, prompt: string, want: string]> = [
    ["whitespace collapses", "fix\tthe\n\n  tab bar\n", "fix the tab bar"],
    ["ends are trimmed", "   spaced   ", "spaced"],
    ["a long prompt keeps 199 characters and an ellipsis", "x".repeat(500), `${"x".repeat(199)}…`],
    ["a prompt of exactly 200 is left alone", "y".repeat(200), "y".repeat(200)],
  ];
  for (const [name, prompt, want] of cases) {
    const pi = await startedSession();

    const written = await fire(pi, "before_agent_start", { prompt });

    assert.deepEqual(written, [["AGENT_MSG", want]], name);
  }
});

test("an empty prompt publishes nothing", async () => {
  const pi = await startedSession();

  assert.deepEqual(await fire(pi, "before_agent_start", { prompt: "" }), []);
  assert.deepEqual(await fire(pi, "before_agent_start", { prompt: "   \n  " }), []);
});

test("a state already published is not published again", async () => {
  // pi fires agent_start once per run, and the tab bar would restart its
  // elapsed counter on a state that never changed.
  const pi = await startedSession();
  await fire(pi, "agent_start");

  assert.deepEqual(await fire(pi, "agent_start"), []);
  assert.deepEqual(await fire(pi, "agent_settled"), [["AGENT_STATE", "idle"]]);
  assert.deepEqual(await fire(pi, "agent_settled"), []);
});

test("an interactive tool blocks and unblocks", async () => {
  const pi = await startedSession();
  await fire(pi, "agent_start");

  const blocked = await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });
  const answered = await fire(pi, "tool_execution_end", { toolName: "ask_user_question" });

  assert.deepEqual(blocked, [["AGENT_STATE", "blocked"]]);
  assert.deepEqual(answered, [["AGENT_STATE", "working"]]);
});

test("nested questions stay blocked until the last one is answered", async () => {
  const pi = await startedSession();
  await fire(pi, "agent_start");
  await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });
  await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });

  const first = await fire(pi, "tool_execution_end", { toolName: "ask_user_question" });
  const second = await fire(pi, "tool_execution_end", { toolName: "ask_user_question" });

  assert.deepEqual(first, [], "one answer left, the agent is still waiting");
  assert.deepEqual(second, [["AGENT_STATE", "working"]]);
});

test("an ordinary tool does not touch the state", async () => {
  const pi = await startedSession();
  await fire(pi, "agent_start");

  assert.deepEqual(await fire(pi, "tool_execution_start", { toolName: "bash" }), []);
  assert.deepEqual(await fire(pi, "tool_execution_end", { toolName: "bash" }), []);
});

test("a new run clears a question left open by the previous one", async () => {
  // pi auto-retries, which starts another run without settling the last one.
  // Interrupting the agent mid-question ends that run with no
  // tool_execution_end, so the count of open questions has to reset here. Left
  // standing, it stops the next answer bringing the agent back to working.
  const pi = await startedSession();
  await fire(pi, "agent_start");
  await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });

  const restarted = await fire(pi, "agent_start");
  await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });
  const answered = await fire(pi, "tool_execution_end", { toolName: "ask_user_question" });

  assert.deepEqual(restarted, [["AGENT_STATE", "working"]]);
  assert.deepEqual(answered, [["AGENT_STATE", "working"]]);
});

test("shutdown clears the state and the message but keeps the kind", async () => {
  const pi = await startedSession();
  await fire(pi, "agent_start");

  const written = await fire(pi, "session_shutdown");

  assert.deepEqual(written, [
    ["AGENT_STATE", null],
    // AGENT_KIND stays, so a quick resume keeps the tag. AGENT_RESUME is
    // missing from this list too, because a session that just ended is the one
    // worth restoring after a reboot.
    ["AGENT_MSG", null],
  ]);
});
