/**
 * Unit tests for extensions/cattery.ts, the pi extension.
 *
 * The extension talks to kitty by writing an OSC escape to stdout, and it
 * learns what the agent is doing from pi lifecycle events. Both ends are
 * replaced here: `FakePi` records the handlers the extension registers and
 * fires them on demand, and `fire` swaps `process.stdout.write` for the
 * duration of one event so the escapes it produces are decoded rather than
 * printed.
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
    // Anything else on stdout would land in the middle of pi's TUI.
    throw new Error(`not a SetUserVar escape: ${JSON.stringify(chunk)}`);
  }
  const [, key, encoded] = match;
  return [key!, encoded === undefined ? null : Buffer.from(encoded, "base64").toString("utf-8")];
}

type Handler = (event: Record<string, unknown>) => Promise<unknown>;

class FakePi {
  readonly handlers = new Map<string, Handler>();

  on(event: string, handler: Handler): void {
    this.handlers.set(event, handler);
  }

  async emit(event: string, payload: Record<string, unknown>): Promise<void> {
    const handler = this.handlers.get(event);
    assert.ok(handler !== undefined, `the extension registered no handler for ${event}`);
    await handler({ type: event, ...payload });
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
 * session to reset them, which is what pi does anyway.
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
    // A previous agent in this window may have left one behind.
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
  // pi fires agent_start per run, and the tab bar would otherwise restart its
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
  // tool_execution_end, so the count of open questions has to reset here or
  // the next answer never brings the agent back to working.
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
    // AGENT_KIND stays, so a quick resume keeps the tag.
    ["AGENT_MSG", null],
  ]);
});
