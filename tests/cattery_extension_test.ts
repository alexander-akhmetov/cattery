/**
 * Unit tests for extensions/cattery.ts, the pi extension.
 *
 * The extension talks to kitty by writing an OSC escape to stdout, and learns
 * what the agent is doing from pi lifecycle events. Both ends are replaced
 * here. `FakePi` records the handlers the extension registers and fires them on
 * demand. `fire` swaps `process.stdout.write` for one event, so the escapes are
 * decoded instead of printed.
 *
 * The tool label is published behind a debounce, so the tests that want one run
 * under node:test's mocked timers and move the clock rather than wait.
 *
 * Run with `make test-ts`, or `npm test`.
 */

import assert from "node:assert/strict";
import test, { afterEach, beforeEach, type TestContext } from "node:test";

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

import register, { resumePrefix, setQueryRunner, setRunner } from "../extensions/cattery.ts";

const defaultQueryRunner = async () => JSON.stringify("pi");
setQueryRunner(defaultQueryRunner);

let savedEnv: NodeJS.ProcessEnv;
beforeEach(() => {
  savedEnv = { ...process.env };
  delete process.env.CATTERY_BIN;
  delete process.env.CATTERY_RESUME_PREFIX;
  delete process.env.CATTERY_RESUME_PREFIX_PI;
  setQueryRunner(defaultQueryRunner);
});
afterEach(() => {
  process.env = savedEnv;
  setQueryRunner(defaultQueryRunner);
});

/** One user variable write. A null value means the variable was deleted. */
type VarWrite = [key: string, value: string | null];

// One chunk can carry several escapes: the extension batches a publish into one
// write, so a tool boundary is one OSC run rather than two.
const SET_USER_VAR = /\x1b\]1337;SetUserVar=([A-Z_]+)(?:=([A-Za-z0-9+/=]*))?\x07/g;

function parseSetUserVars(chunk: string): VarWrite[] {
  const out: VarWrite[] = [];
  let consumed = 0;
  for (const match of chunk.matchAll(SET_USER_VAR)) {
    if (match.index !== consumed) break;
    consumed += match[0].length;
    const [, key, encoded] = match;
    out.push([key!, encoded === undefined ? null : Buffer.from(encoded, "base64").toString("utf-8")]);
  }
  if (consumed !== chunk.length) {
    // Anything else on stdout would appear in the middle of pi's TUI.
    throw new Error(`not a SetUserVar escape run: ${JSON.stringify(chunk)}`);
  }
  return out;
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

/**
 * Put the process in a kitty window and nowhere else.
 *
 * The suite can be run from inside a tmux pane, and a pane beats a kitty window
 * in the extension, so the kitty tests have to say so.
 */
function inKittyOnly(): void {
  delete process.env.TMUX;
  delete process.env.TMUX_PANE;
}

/** Run `fn` with stdout captured, and return the user variables it wrote. */
async function captured(fn: () => Promise<void> | void, written: VarWrite[] = []): Promise<VarWrite[]> {
  const original = process.stdout.write;
  process.stdout.write = ((chunk: string | Uint8Array): boolean => {
    written.push(...parseSetUserVars(typeof chunk === "string" ? chunk : Buffer.from(chunk).toString("utf-8")));
    return true;
  }) as typeof process.stdout.write;
  try {
    await fn();
  } finally {
    process.stdout.write = original;
  }
  return written;
}

/** Fire one pi event and return the user variables the extension wrote. */
async function fire(pi: FakePi, event: string, payload: Record<string, unknown> = {}): Promise<VarWrite[]> {
  return captured(() => pi.emit(event, payload));
}

// Long enough to pass the extension's tool debounce. The tests move a mocked
// clock rather than wait, so this only has to be past it, not equal to it.
const PAST_DEBOUNCE_MS = 5_000;

/**
 * A registered extension in a window that has already started its session.
 *
 * The extension keeps its state in module variables, so every test starts a
 * session to reset them, as pi does.
 */
async function startedSession(): Promise<FakePi> {
  inKittyOnly();
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  register(pi as unknown as ExtensionAPI);
  await fire(pi, "session_start");
  return pi;
}

test("outside kitty and tmux the extension registers nothing", () => {
  const saved = { ...process.env };
  delete process.env.KITTY_WINDOW_ID;
  delete process.env.TMUX;
  delete process.env.TMUX_PANE;
  try {
    const pi = new FakePi();
    register(pi as unknown as ExtensionAPI);
    assert.equal(pi.handlers.size, 0);
  } finally {
    process.env = saved;
  }
});

test("a session opens as an idle pi agent with no stale message", async () => {
  inKittyOnly();
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  register(pi as unknown as ExtensionAPI);

  const written = await fire(pi, "session_start");

  assert.deepEqual(written, [
    ["AGENT_KIND", "pi"],
    ["AGENT_RESUME", `pi --session ${SESSION_FILE}`],
    // A previous agent in this window may have left these behind.
    ["AGENT_MSG", null],
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
    ["AGENT_STATE", "idle"],
  ]);
});

/** Fire session_start with a given session file and return only AGENT_RESUME. */
async function resumeOn(sessionFile: string | undefined): Promise<VarWrite[]> {
  inKittyOnly();
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  pi.ctx = context(sessionFile);
  register(pi as unknown as ExtensionAPI);
  const written = await fire(pi, "session_start");
  return written.filter(([key]) => key === "AGENT_RESUME");
}

test("the prefix query sets execFile limits and CATTERY_BIN without a shell", async () => {
  for (const binary of [undefined, "", "/tmp/cattery bin; literal"]) {
    if (binary === undefined) delete process.env.CATTERY_BIN;
    else process.env.CATTERY_BIN = binary;
    let calls = 0;
    setQueryRunner(async (file, args, options) => {
      calls += 1;
      assert.equal(file, binary || "cattery");
      assert.deepEqual(args, ["config", "resume-prefix", "pi"]);
      assert.deepEqual(options, { encoding: "utf8", timeout: 1000, maxBuffer: 1024 * 1024, shell: false });
      return '  "nono run pi --profile personal"\n';
    });
    assert.equal(await resumePrefix(), "nono run pi --profile personal");
    assert.equal(calls, 1);
  }
});

test("query failures silently use the default prefix", async () => {
  const failures = [
    new Error("missing binary"),
    Object.assign(new Error("not found"), { code: "ENOENT" }),
    Object.assign(new Error("old binary or bad config"), { code: 1, stdout: '"ignored"' }),
    Object.assign(new Error("timeout"), { killed: true, signal: "SIGTERM" }),
    Object.assign(new Error("buffer exceeded"), { code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" }),
  ];
  for (const error of failures) {
    setQueryRunner(async () => {
      throw error;
    });
    assert.equal(await resumePrefix(), "pi", error.message);
  }
  setQueryRunner(() => {
    throw new Error("synchronous failure");
  });
  assert.equal(await resumePrefix(), "pi");
});

test("the query accepts only one nonempty JSON string without control characters", async () => {
  const malformed = [
    "",
    "pi",
    '"pi" trailing',
    '"pi"\n"other"',
    "null",
    "false",
    "42",
    "[]",
    '{"prefix":"pi"}',
    JSON.stringify(""),
    JSON.stringify("\t"),
    ...[0, 10, 13, 27, 31, 127, 133, 159].map((code) => JSON.stringify(`pi${String.fromCharCode(code)}run`)),
  ];
  for (const stdout of malformed) {
    setQueryRunner(async () => stdout);
    assert.equal(await resumePrefix(), "pi", JSON.stringify(stdout));
  }
  for (const prefix of ["nono run pi", " pi --profile personal ", "   ", "env LABEL='猫' pi"]) {
    setQueryRunner(async () => JSON.stringify(prefix));
    assert.equal(await resumePrefix(), prefix);
  }
});

test("nonempty environment overrides skip the query, including whitespace", async () => {
  let calls = 0;
  setQueryRunner(async () => {
    calls += 1;
    throw new Error("must not query");
  });
  assert.deepEqual(await resumeWithPrefix("shared pi"), [["AGENT_RESUME", `shared pi --session ${SESSION_FILE}`]]);
  assert.deepEqual(await resumeWithPrefix("shared pi", "specific pi"), [
    ["AGENT_RESUME", `specific pi --session ${SESSION_FILE}`],
  ]);
  assert.deepEqual(await resumeWithPrefix("shared pi", ""), [["AGENT_RESUME", `shared pi --session ${SESSION_FILE}`]]);
  assert.deepEqual(await resumeWithPrefix("", " "), [["AGENT_RESUME", `  --session ${SESSION_FILE}`]]);
  assert.equal(calls, 0);
});

test("empty environment overrides still query the configured prefix", async () => {
  let calls = 0;
  setQueryRunner(async () => {
    calls += 1;
    return JSON.stringify("configured pi");
  });
  assert.deepEqual(await resumeWithPrefix("", ""), [["AGENT_RESUME", `configured pi --session ${SESSION_FILE}`]]);
  assert.equal(calls, 1);
});

test("the configured prefix stays raw and the session path is shell quoted", async () => {
  setQueryRunner(async () => JSON.stringify("env PROFILE='my profile' nono run pi"));
  assert.deepEqual(await resumeOn("/tmp/a b'c.jsonl"), [
    ["AGENT_RESUME", "env PROFILE='my profile' nono run pi --session '/tmp/a b'\\''c.jsonl'"],
  ]);
});

test("ephemeral and unreadable session paths skip the query", async () => {
  let calls = 0;
  setQueryRunner(async () => {
    calls += 1;
    throw new Error("must not query");
  });
  for (const file of [undefined, ""]) {
    assert.deepEqual(await resumeOn(file), [["AGENT_RESUME", null]]);
  }
  inKittyOnly();
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  pi.ctx = {
    sessionManager: {
      getSessionFile: () => {
        throw new Error("unreadable path");
      },
    },
  };
  register(pi as unknown as ExtensionAPI);
  const written = await fire(pi, "session_start");
  assert.deepEqual(written, [
    ["AGENT_KIND", "pi"],
    ["AGENT_RESUME", null],
    ["AGENT_MSG", null],
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
    ["AGENT_STATE", "idle"],
  ]);
  assert.equal(calls, 0);
});

test("each session start queries again, but run and shutdown events do not", async () => {
  let calls = 0;
  setQueryRunner(async () => JSON.stringify(`configured-${++calls}`));
  const pi = await startedSession();
  assert.equal(calls, 1);
  for (const reason of ["new", "resume", "fork", "reload"]) {
    pi.ctx = context(`/tmp/${reason}.jsonl`);
    const written = await fire(pi, "session_start", { reason });
    assert.deepEqual(written[1], ["AGENT_RESUME", `configured-${calls} --session /tmp/${reason}.jsonl`]);
    assert.equal(written.at(-1)?.[0], "AGENT_STATE");
  }
  assert.equal(calls, 5);
  await fire(pi, "before_agent_start", { prompt: "work" });
  await fire(pi, "agent_start");
  await fire(pi, "tool_execution_start", { toolCallId: "c1", toolName: "read" });
  await fire(pi, "tool_execution_end", { toolCallId: "c1", toolName: "read" });
  await fire(pi, "agent_settled");
  await fire(pi, "session_shutdown");
  assert.equal(calls, 5);
});

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
  inKittyOnly();
  process.env.KITTY_WINDOW_ID = "7";
  const pi = new FakePi();
  pi.ctx = undefined as unknown as FakeContext;
  register(pi as unknown as ExtensionAPI);

  const written = await fire(pi, "session_start");

  assert.deepEqual(written, [
    ["AGENT_KIND", "pi"],
    ["AGENT_RESUME", null],
    ["AGENT_MSG", null],
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
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

  assert.deepEqual(await fire(pi, "tool_execution_start", { toolCallId: "c1", toolName: "bash" }), []);
  assert.deepEqual(await fire(pi, "tool_execution_end", { toolCallId: "c1", toolName: "bash" }), []);
});

// --- the running tool ---------------------------------------------------------

type Timers = TestContext["mock"]["timers"];

/**
 * Start a session with the tool debounce under the test's own clock.
 *
 * node:test restores the clock at the end of each test, and enable() refuses a
 * second call, so a test that starts several sessions calls this once and
 * startedSession after it.
 */
async function toolSession(timers: Timers): Promise<FakePi> {
  timers.enable({ apis: ["setTimeout"] });
  return startedSession();
}

/** Fire pi events, then let the debounce elapse, and return what was written. */
async function fireTools(
  timers: Timers,
  pi: FakePi,
  events: Array<[event: string, payload: Record<string, unknown>]>,
): Promise<VarWrite[]> {
  return captured(async () => {
    for (const [event, payload] of events) await pi.emit(event, payload);
    timers.tick(PAST_DEBOUNCE_MS);
  });
}

function start(id: string, toolName: string, args?: Record<string, unknown>): [string, Record<string, unknown>] {
  return ["tool_execution_start", { toolCallId: id, toolName, args }];
}

function end(id: string, toolName: string): [string, Record<string, unknown>] {
  return ["tool_execution_end", { toolCallId: id, toolName }];
}

test("session_start cancels prior tool publication before awaiting the prefix query", async (t) => {
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");
  await fireTools(t.mock.timers, pi, [start("old", "bash", { command: "sleep 900" })]);
  await fire(pi, "tool_execution_start", { toolCallId: "pending", toolName: "read" });
  await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });
  await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });
  pi.ctx = context("/tmp/new session.jsonl");

  let resolveQuery!: (stdout: string) => void;
  const query = new Promise<string>((resolve) => {
    resolveQuery = resolve;
  });
  const written: VarWrite[] = [];
  let calls = 0;
  setQueryRunner(() => {
    calls += 1;
    assert.deepEqual(written, []);
    return query;
  });
  await captured(async () => {
    const starting = pi.emit("session_start", {});
    assert.equal(calls, 1);
    assert.deepEqual(written, []);
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
    await Promise.resolve();
    assert.deepEqual(written, [], "neither the prior tool nor the new session may publish during lookup");
    resolveQuery(JSON.stringify("configured pi"));
    await starting;
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
  }, written);

  assert.deepEqual(written, [
    ["AGENT_KIND", "pi"],
    ["AGENT_RESUME", "configured pi --session '/tmp/new session.jsonl'"],
    ["AGENT_MSG", null],
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
    ["AGENT_STATE", "idle"],
  ]);
  assert.deepEqual(await fireTools(t.mock.timers, pi, [end("old", "bash"), end("pending", "read")]), []);
  assert.deepEqual(await fire(pi, "agent_start"), [["AGENT_STATE", "working"]]);
  await fire(pi, "tool_execution_start", { toolName: "ask_user_question" });
  assert.deepEqual(await fire(pi, "tool_execution_end", { toolName: "ask_user_question" }), [
    ["AGENT_STATE", "working"],
  ]);
});

for (const host of ["kitty", "tmux"]) {
  test(`${host}: prefix lookup preserves prompts, tools, and state published during startup`, async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const pi = host === "kitty" ? await startedSession() : inPane().pi;
    if (host === "tmux") await firePane(pi, "session_start");
    for (const phase of ["prompt", "working", "pending tool", "published tool", "blocked", "settled"]) {
      pi.ctx = context(`/tmp/${phase}.jsonl`);
      let resolveQuery!: (stdout: string) => void;
      setQueryRunner(() => new Promise((resolve) => {
        resolveQuery = resolve;
      }));
      await capturedHost(host, async () => {
        const starting = pi.emit("session_start", {});
        await pi.emit("before_agent_start", { prompt: "new request" });
        if (phase !== "prompt") await pi.emit("agent_start", {});
        if (phase.includes("tool")) {
          await pi.emit(...start("current", "bash", { command: "sleep 900" }));
          if (phase === "published tool") t.mock.timers.tick(PAST_DEBOUNCE_MS);
        }
        if (phase === "blocked") {
          await pi.emit("tool_execution_start", { toolName: "ask_user_question" });
        }
        if (phase === "settled") await pi.emit("agent_settled", {});
        const completion = await capturedHost(host, async () => {
          resolveQuery(JSON.stringify("configured pi"));
          await starting;
        });
        assert.ok(completion.some(([key, value]) => key === "AGENT_RESUME" && value?.includes("configured pi")));
        assert.deepEqual(completion.filter(([key]) => key === "AGENT_MSG"), [["AGENT_MSG", "new request"]], phase);
        if (phase !== "prompt") {
          assert.deepEqual(completion.filter(([key]) => key === "AGENT_STATE" || key === "AGENT_WORKED"), [], phase);
        }
        if (phase === "published tool") {
          assert.deepEqual(completion.filter(([key]) => key.startsWith("AGENT_TOOL")), []);
        }
        if (phase === "pending tool") {
          const delayed = await capturedHost(host, () => t.mock.timers.tick(PAST_DEBOUNCE_MS));
          assert.ok(delayed.some(([key, value]) => key === "AGENT_TOOL" && value === "bash: sleep 900"));
        }
        if (phase === "blocked") {
          const reply = await capturedHost(host, () => pi.emit("tool_execution_end", { toolName: "ask_user_question" }));
          assert.ok(reply.some(([key, value]) => key === "AGENT_STATE" && value === "working"));
        }
      });
    }
  });

  test(`${host}: superseded prefix queries cannot publish after replacement or shutdown`, async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    for (const replacement of ["same runtime", "new runtime", "shutdown only"]) {
      const pi = host === "kitty" ? await startedSession() : inPane().pi;
      if (host === "tmux") await firePane(pi, "session_start");
      let resolveOld!: (stdout: string) => void;
      setQueryRunner(() => new Promise((resolve) => {
        resolveOld = resolve;
      }));
      await capturedHost(host, async () => {
        pi.ctx = context("/tmp/old.jsonl");
        const oldStart = pi.emit("session_start", {});
        if (replacement !== "same runtime") await pi.emit("session_shutdown", {});
        if (replacement !== "shutdown only") {
          const current = replacement === "new runtime" ? new FakePi() : pi;
          if (current !== pi) register(current as unknown as ExtensionAPI);
          current.ctx = context("/tmp/current.jsonl");
          setQueryRunner(defaultQueryRunner);
          await current.emit("session_start", {});
          await current.emit("before_agent_start", { prompt: "current request" });
          await current.emit("agent_start", {});
          await current.emit(...start("current", "bash", { command: "sleep 900" }));
          t.mock.timers.tick(PAST_DEBOUNCE_MS);
        }
        const stale = await capturedHost(host, async () => {
          resolveOld(JSON.stringify("old pi"));
          await oldStart;
          t.mock.timers.tick(PAST_DEBOUNCE_MS);
        });
        assert.deepEqual(stale, [], replacement);
      });
    }
  });
}

test("a running tool is published with the second it started", async (t) => {
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");
  const before = Math.floor(Date.now() / 1000);

  const written = await fireTools(t.mock.timers, pi, [start("c1", "bash", { command: "go test ./..." })]);

  // The timestamp goes first: AGENT_TOOL is the key that wakes the kitty
  // watcher, and the other order would have it read the previous tool's stamp.
  assert.equal(written.length, 2);
  const [sinceKey, since] = written[0]!;
  assert.equal(sinceKey, "AGENT_TOOL_SINCE");
  assert.ok(Number(since) >= before && Number(since) <= Math.floor(Date.now() / 1000), `stamp ${since}`);
  assert.deepEqual(written[1], ["AGENT_TOOL", "bash: go test ./..."]);
});

test("the label names the argument that says what the tool is doing", async (t) => {
  const cases: Array<[name: string, args: Record<string, unknown> | undefined, want: string]> = [
    ["a path", { path: "/tmp/x.go" }, "read: /tmp/x.go"],
    ["no known argument", { offset: 3 }, "read"],
    ["no arguments at all", undefined, "read"],
    // An extension can register a tool taking anything, and a number is not a
    // label.
    ["a non-string argument", { path: 7 }, "read"],
    ["whitespace collapses", { path: "a\n  b" }, "read: a b"],
    ["a long argument is capped", { path: "x".repeat(300) }, `read: ${"x".repeat(113)}…`],
  ];
  t.mock.timers.enable({ apis: ["setTimeout"] });
  for (const [name, args, want] of cases) {
    const pi = await startedSession();
    await fire(pi, "agent_start");

    const written = await fireTools(t.mock.timers, pi, [start("c1", "read", args)]);

    assert.deepEqual(written[1], ["AGENT_TOOL", want], name);
  }
});

test("two calls in flight report the earliest, and promote the next one", async (t) => {
  // pi runs siblings concurrently, and an immediate failure can end before a
  // sibling starts. Publishing the newest would let a fast read restamp the
  // timestamp of a bash that has hung for 19 minutes.
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");

  const both = await fireTools(t.mock.timers, pi, [
    start("c1", "read", { path: "/tmp/x.go" }),
    start("c2", "bash", { command: "go test ./..." }),
  ]);
  const promoted = await fireTools(t.mock.timers, pi, [end("c1", "read")]);
  const cleared = await fireTools(t.mock.timers, pi, [end("c2", "bash")]);

  assert.deepEqual(both[1], ["AGENT_TOOL", "read: /tmp/x.go"]);
  assert.deepEqual(promoted[1], ["AGENT_TOOL", "bash: go test ./..."]);
  assert.deepEqual(cleared, [
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
  ]);
});

test("a tool shorter than the debounce is never published", async (t) => {
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");

  const written = await fireTools(t.mock.timers, pi, [start("c1", "read", { path: "/tmp/x.go" }), end("c1", "read")]);

  assert.deepEqual(written, []);
});

test("an interactive tool publishes no label", async (t) => {
  // ask_user_question sets "blocked", which already carries its own elapsed
  // time through AGENT_SINCE. A question is not a stall.
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");

  const written = await fireTools(t.mock.timers, pi, [start("c1", "ask_user_question", { question: "which?" })]);

  assert.deepEqual(written, [["AGENT_STATE", "blocked"]]);
});

test("a new run clears a tool left open by an interrupt", async (t) => {
  // An interrupt tears the process down with no tool_execution_end. Left
  // standing, the label would keep an hours-old timestamp and the next run
  // would read as stalled from its first second.
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");
  await fireTools(t.mock.timers, pi, [start("c1", "bash", { command: "sleep 900" })]);

  const restarted = await fireTools(t.mock.timers, pi, [["agent_start", {}]]);

  // AGENT_STATE is missing because the agent never left "working": pi
  // auto-retries, and a run can start again without settling the last one.
  assert.deepEqual(restarted, [
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
  ]);
});

test("settling clears the tool before it publishes idle", async (t) => {
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");
  await fireTools(t.mock.timers, pi, [start("c1", "bash", { command: "sleep 900" })]);

  const settled = await fireTools(t.mock.timers, pi, [["agent_settled", {}]]);

  assert.deepEqual(settled, [
    ["AGENT_TOOL_SINCE", null],
    ["AGENT_TOOL", null],
    ["AGENT_STATE", "idle"],
  ]);
});

test("a value carrying a control character publishes without it", async (t) => {
  // \x1f separates the fields of a tmux list-panes row, and the picker drops a
  // row whose field count is wrong: the agent would leave the picker rather
  // than show bad text.
  const pi = await toolSession(t.mock.timers);
  await fire(pi, "agent_start");

  const written = await fireTools(t.mock.timers, pi, [start("c1", "bash", { command: "printf 'a\x1fb'" })]);

  assert.deepEqual(written[1], ["AGENT_TOOL", "bash: printf 'a b'"]);
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
    // AGENT_MSG first: clearing AGENT_STATE wakes the watcher, and the event it
    // emits then carries no prompt from the session that just ended.
    ["AGENT_MSG", null],
    // AGENT_KIND stays, so a quick resume keeps the tag. AGENT_RESUME is
    // missing from this list too, because a session that just ended is the one
    // worth restoring after a reboot. So is the tool pair, which this run never
    // published.
    ["AGENT_STATE", null],
  ]);
});

// --- tmux panes ---------------------------------------------------------------

/** One tmux run: the argv the extension passed, without the binary name. */
type TmuxRun = string[];

/**
 * A registered extension in a tmux pane, with the process runner replaced.
 *
 * The returned array collects every tmux command line, so the tests assert the
 * options without a tmux server.
 */
function inPane(): { pi: FakePi; runs: TmuxRun[] } {
  process.env.TMUX = "/private/tmp/tmux-501/default,69427,0";
  process.env.TMUX_PANE = "%17";
  // A dev agent inherits this from the tmux server, which inherited it from
  // whatever kitty window started the daemon. It names an unrelated window.
  process.env.KITTY_WINDOW_ID = "7";
  const runs: TmuxRun[] = [];
  setRunner((file, args) => {
    assert.equal(file, "tmux");
    runs.push(args);
  });
  const pi = new FakePi();
  register(pi as unknown as ExtensionAPI);
  return { pi, runs };
}

/** Run `fn` in a pane and return the tmux command lines it ran. */
async function capturedRuns(fn: () => Promise<void> | void): Promise<TmuxRun[]> {
  const runs: TmuxRun[] = [];
  setRunner((_file, args) => {
    runs.push(args);
  });
  const original = process.stdout.write;
  process.stdout.write = ((): boolean => {
    throw new Error("wrote an escape sequence from a tmux pane");
  }) as typeof process.stdout.write;
  try {
    await fn();
  } finally {
    process.stdout.write = original;
  }
  return runs;
}

async function capturedHost(host: string, fn: () => Promise<void> | void): Promise<VarWrite[]> {
  if (host === "kitty") return captured(fn);
  const writes: VarWrite[] = [];
  for (const args of await capturedRuns(fn)) {
    let command: string[] = [];
    for (const arg of [...args, ";"]) {
      if (arg !== ";") {
        command.push(arg);
        continue;
      }
      const key = command.findIndex((value) => value.startsWith("@"));
      assert.ok(key >= 0);
      writes.push([command[key]!.slice(1), command.includes("-u") ? null : command[key + 1]!]);
      command = [];
    }
  }
  return writes;
}

/** Fire one pi event in a pane and return the tmux command lines it ran. */
async function firePane(pi: FakePi, event: string, payload: Record<string, unknown> = {}): Promise<TmuxRun[]> {
  return capturedRuns(() => pi.emit(event, payload));
}

/** The options one tmux command line sets, deletions marked with "-u". */
function options(args: TmuxRun): string[] {
  const out: string[] = [];
  let deleting = false;
  for (const arg of args) {
    if (arg === ";") deleting = false;
    else if (arg === "-u") deleting = true;
    else if (arg.startsWith("@")) out.push(deleting ? `-u ${arg}` : arg);
  }
  return out;
}

test("in a pane the extension publishes options, not escapes", async () => {
  const { pi } = inPane();

  const runs = await firePane(pi, "session_start");

  assert.deepEqual(runs.slice(0, 4), [
    ["set", "-p", "-t", "%17", "@AGENT_KIND", "pi"],
    ["set", "-p", "-t", "%17", "@AGENT_RESUME", `pi --session ${SESSION_FILE}`],
    // A pi that was killed leaves "has worked" behind, and this session's first
    // idle would read as "done" in the picker.
    ["set", "-p", "-u", "-t", "%17", "@AGENT_WORKED"],
    ["set", "-p", "-u", "-t", "%17", "@AGENT_MSG"],
  ]);
  // The same goes for the tool it was running, which nothing else clears.
  assert.deepEqual(options(runs[4]!), ["-u @AGENT_TOOL_SINCE", "-u @AGENT_TOOL"]);
  // The state chains the stamp the picker counts elapsed time from into the
  // same command line, so a state change costs one process.
  assert.deepEqual(runs[5]!.slice(0, 7), ["set", "-p", "-t", "%17", "@AGENT_STATE", "idle", ";"]);
  assert.deepEqual(options(runs[5]!), ["@AGENT_STATE", "@AGENT_SINCE"]);
});

test("a pane publishes nothing until the prefix query resolves, then publishes state last", async (t) => {
  const { pi } = inPane();
  t.mock.timers.enable({ apis: ["setTimeout"] });
  await firePane(pi, "session_start");
  await firePane(pi, "agent_start");
  await firePane(pi, "tool_execution_start", { toolCallId: "old", toolName: "bash" });
  let resolveQuery!: (stdout: string) => void;
  const query = new Promise<string>((resolve) => {
    resolveQuery = resolve;
  });
  setQueryRunner(() => query);
  const runs: TmuxRun[] = [];
  setRunner((file, args) => {
    assert.equal(file, "tmux");
    runs.push(args);
  });

  const written = await captured(async () => {
    const starting = pi.emit("session_start", {});
    assert.deepEqual(runs, []);
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
    await Promise.resolve();
    assert.deepEqual(runs, []);
    resolveQuery(JSON.stringify("nono run pi"));
    await starting;
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
  });

  assert.deepEqual(written, []);
  assert.equal(runs.length, 6);
  assert.equal(runs[1]!.at(-1), `nono run pi --session ${SESSION_FILE}`);
  assert.deepEqual(runs.map(options), [
    ["@AGENT_KIND"],
    ["@AGENT_RESUME"],
    ["-u @AGENT_WORKED"],
    ["-u @AGENT_MSG"],
    ["-u @AGENT_TOOL_SINCE", "-u @AGENT_TOOL"],
    ["@AGENT_STATE", "@AGENT_SINCE"],
  ]);
});

// tmux ends a command at any argument ending in ";", so a prompt ending in one
// would lose that character, and a prompt that is only ";" would swallow the
// updates chained behind it. This mirrors escapeArg in internal/state/tmux.go.
test("a prompt that ends in a semicolon is escaped", async () => {
  const { pi } = inPane();
  await firePane(pi, "session_start");

  const plain = await firePane(pi, "before_agent_start", { prompt: "cd x; make" });
  const trailing = await firePane(pi, "before_agent_start", { prompt: "make test;" });
  const only = await firePane(pi, "before_agent_start", { prompt: ";" });

  assert.deepEqual(plain[0], ["set", "-p", "-t", "%17", "@AGENT_MSG", "cd x; make"]);
  assert.deepEqual(trailing[0], ["set", "-p", "-t", "%17", "@AGENT_MSG", "make test\\;"]);
  assert.deepEqual(only[0], ["set", "-p", "-t", "%17", "@AGENT_MSG", "\\;"]);
});

test("a pane with no kitty window at all still publishes", async () => {
  // A tmux server started outside kitty gives its panes no KITTY_WINDOW_ID, and
  // the OSC path has nowhere to go.
  const { pi } = inPane();
  delete process.env.KITTY_WINDOW_ID;

  const runs = await firePane(pi, "session_start");

  assert.equal(runs.length, 6);
});

test("the pane options a state change leaves behind", async () => {
  const { pi } = inPane();
  await firePane(pi, "session_start");

  const working = await firePane(pi, "agent_start");
  const blocked = await firePane(pi, "tool_execution_start", { toolName: "ask_user_question" });
  const idle = await firePane(pi, "agent_settled");
  const shutdown = await firePane(pi, "session_shutdown");

  // Working and blocked mark the work and drop the acknowledgement, so the next
  // idle reads as "done" until someone looks at the pane.
  assert.deepEqual(options(working[0]!), ["@AGENT_STATE", "@AGENT_SINCE", "@AGENT_WORKED", "-u @AGENT_SEEN"]);
  assert.deepEqual(options(blocked[0]!), ["@AGENT_STATE", "@AGENT_SINCE", "@AGENT_WORKED", "-u @AGENT_SEEN"]);
  assert.deepEqual(options(idle[0]!), ["@AGENT_STATE", "@AGENT_SINCE"]);
  // Shutdown forgets the work, or the next agent in this pane would report
  // "done" the moment it starts idle. AGENT_RESUME stays.
  assert.deepEqual(options(shutdown[0]!), ["-u @AGENT_MSG"]);
  assert.deepEqual(options(shutdown[1]!), ["-u @AGENT_STATE", "-u @AGENT_WORKED"]);
});

test("a state already published is not published again in a pane", async () => {
  const { pi } = inPane();
  await firePane(pi, "session_start");
  await firePane(pi, "agent_start");

  assert.deepEqual(await firePane(pi, "agent_start"), []);
});

test("a tool boundary in a pane costs one process", async (t) => {
  // Every write in a pane is a fork on pi's main thread, so the pair goes out
  // as one chained command rather than two.
  const { pi } = inPane();
  t.mock.timers.enable({ apis: ["setTimeout"] });
  await firePane(pi, "session_start");
  await firePane(pi, "agent_start");

  const runs = await capturedRuns(async () => {
    await pi.emit("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: { command: "go test ./..." } });
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
  });

  assert.equal(runs.length, 1);
  assert.deepEqual(options(runs[0]!), ["@AGENT_TOOL_SINCE", "@AGENT_TOOL"]);
  assert.equal(runs[0]!.at(-1), "bash: go test ./...");
});

test("a tool label that ends in a semicolon is escaped", async (t) => {
  // tmux ends a command at any argument ending in ";", so the label would lose
  // that character and take the option chained behind it with it.
  const { pi } = inPane();
  t.mock.timers.enable({ apis: ["setTimeout"] });
  await firePane(pi, "session_start");
  await firePane(pi, "agent_start");

  const runs = await capturedRuns(async () => {
    await pi.emit("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: { command: "make test;" } });
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
  });

  assert.equal(runs[0]!.at(-1), "bash: make test\\;");
});

test("@AGENT_SINCE is unix seconds", async () => {
  const { pi } = inPane();
  await firePane(pi, "session_start");
  const before = Math.floor(Date.now() / 1000);

  const runs = await firePane(pi, "agent_start");

  const args = runs[0]!;
  const stamp = Number(args[args.indexOf("@AGENT_SINCE") + 1]);
  assert.ok(Number.isInteger(stamp), "@AGENT_SINCE is not an integer");
  assert.ok(stamp >= before && stamp <= Math.floor(Date.now() / 1000), `@AGENT_SINCE ${stamp} is outside the run`);
});
