/**
 * Unit tests for opencode/cattery.ts, the opencode plugin.
 *
 * The plugin learns what the agent is doing from opencode hooks and events, and
 * says so by running `cattery state <word> --kind opencode` with a JSON payload
 * on stdin. Both ends are replaced here. `FakeHost` stands in for the plugin
 * input, answering `client.session.get`, and `setRunner` captures the runs
 * instead of forking a binary.
 *
 * The tool label is published behind a debounce, so the tests that want one run
 * under node:test's mocked timers and move the clock rather than wait.
 *
 * Run with `make test-ts`, or `npm test`.
 */

import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";

import type { Hooks, PluginInput } from "@opencode-ai/plugin";

import plugin, { setRunner } from "../opencode/cattery.ts";

/** One `cattery state` run, decoded. */
type Run = { state: string; payload: Record<string, unknown> };

/**
 * The half of the plugin input the plugin reads, plus the runs it made.
 *
 * `parents` is what the opencode server would answer for a session id the
 * plugin never saw created: the value is the parent session, or undefined for a
 * top-level one. An id absent from the map makes the lookup fail, which is the
 * server being unreachable.
 */
class FakeHost {
  readonly runs: Run[] = [];
  readonly parents = new Map<string, string | undefined>();
  lookups = 0;
  bin = "";

  /** The runs since the last call, so a test reads one transition at a time. */
  take(): Run[] {
    return this.runs.splice(0, this.runs.length);
  }

  /** The last run's payload, which is what most assertions are about. */
  last(): Run {
    const runs = this.take();
    assert.ok(runs.length > 0, "the plugin published nothing");
    return runs[runs.length - 1]!;
  }

  input(): PluginInput {
    return {
      client: {
        session: {
          get: async ({ path }: { path: { id: string } }) => {
            this.lookups += 1;
            if (!this.parents.has(path.id)) throw new Error(`no session ${path.id}`);
            return { data: { id: path.id, parentID: this.parents.get(path.id) } };
          },
        },
      },
    } as unknown as PluginInput;
  }
}

/**
 * Load the plugin in a kitty window, with its runs captured.
 *
 * The suite can be run from inside a tmux pane, and a pane beats a kitty window
 * in the plugin, so the tests have to say which host they mean.
 */
async function load(host: FakeHost): Promise<Hooks> {
  delete process.env.TMUX;
  delete process.env.TMUX_PANE;
  delete process.env.CATTERY_BIN;
  process.env.KITTY_WINDOW_ID = "7";
  setRunner((file, args, input) => {
    host.bin = file;
    assert.deepEqual(args.slice(0, 1).concat(args.slice(2)), ["state", "--kind", "opencode"], `argv ${args}`);
    host.runs.push({ state: args[1]!, payload: JSON.parse(input) as Record<string, unknown> });
  });
  return plugin.server(host.input());
}

/** The session every test uses unless it is testing the session id itself. */
const SESSION = "ses_8a3f";

/** Load the plugin and put it in a turn, with the opening runs drained. */
async function working(host: FakeHost): Promise<Hooks> {
  const hooks = await load(host);
  await created(hooks, SESSION);
  await prompt(hooks, SESSION, "fix the picker");
  host.take();
  return hooks;
}

function fire(hooks: Hooks, type: string, properties: Record<string, unknown>): Promise<void> {
  return hooks.event!({ event: { type, properties } } as never);
}

/** The session.created event opencode publishes when it opens a session. */
function created(hooks: Hooks, id: string, parentID?: string): Promise<void> {
  return fire(hooks, "session.created", { sessionID: id, info: { id, parentID } });
}

function status(hooks: Hooks, id: string, type: string): Promise<void> {
  return fire(hooks, "session.status", { sessionID: id, status: { type } });
}

function prompt(hooks: Hooks, id: string, text: string): Promise<void> {
  return hooks["chat.message"]!({ sessionID: id }, { parts: [{ type: "text", text }] } as never);
}

test("outside kitty and tmux the plugin registers nothing", async () => {
  const saved = { ...process.env };
  delete process.env.KITTY_WINDOW_ID;
  delete process.env.TMUX;
  delete process.env.TMUX_PANE;
  try {
    const host = new FakeHost();
    setRunner(() => assert.fail("published outside a host it can reach"));
    const hooks = await plugin.server(host.input());
    assert.deepEqual(Object.keys(hooks), []);
  } finally {
    process.env = saved;
  }
});

// A file plugin whose default export has no id is skipped, and the TypeError is
// logged rather than surfaced: the agent runs on with no tab marker at all.
test("the default export carries the id a file plugin needs", () => {
  assert.equal(plugin.id, "cattery");
  assert.equal(typeof plugin.server, "function");
});

test("loading opens an idle agent and drops what the last one left", async () => {
  const host = new FakeHost();

  await load(host);

  // "startup" is what makes the Go writer delete AGENT_MSG and @AGENT_WORKED,
  // and the empty tool deletes the pair a killed agent left standing.
  assert.deepEqual(host.take(), [{ state: "idle", payload: { source: "startup", tool: "" } }]);
});

test("the binary comes from CATTERY_BIN when the kitty block exported one", async () => {
  const host = new FakeHost();
  await load(host);
  assert.equal(host.bin, "cattery");

  process.env.CATTERY_BIN = "/opt/homebrew/bin/cattery";
  try {
    setRunner((file) => {
      host.bin = file;
    });
    await plugin.server(host.input());
    assert.equal(host.bin, "/opt/homebrew/bin/cattery");
  } finally {
    delete process.env.CATTERY_BIN;
  }
});

test("a prompt publishes the session, the message and working", async () => {
  const host = new FakeHost();
  const hooks = await load(host);
  await created(hooks, SESSION);
  host.take();

  await prompt(hooks, SESSION, "  fix   the\npicker  ");

  // The bare id, not a command: the Go writer composes
  // "opencode --session <id>" and quotes the id itself.
  assert.deepEqual(host.last(), {
    state: "working",
    payload: { prompt: "fix the picker", tool: "", session_id: SESSION },
  });
});

test("a turn ending publishes idle and clears the tool", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  await status(hooks, SESSION, "idle");
  assert.deepEqual(host.last(), { state: "idle", payload: { tool: "", session_id: SESSION } });

  // A second idle is one fork for a window that already says so, and it would
  // wake the watcher again for nothing.
  await status(hooks, SESSION, "idle");
  assert.deepEqual(host.take(), []);
});

test("a retry is not the turn ending", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  await fire(hooks, "session.status", { sessionID: SESSION, status: { type: "retry", attempt: 2, next: 5 } });

  assert.deepEqual(host.take(), [], "a retry is opencode waiting on a provider, not the agent stopping");
});

test("the same state twice is published once", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  await status(hooks, SESSION, "busy");

  assert.deepEqual(host.take(), [], "chat.message already said working");
});

test("busy after idle starts a new turn", async () => {
  const host = new FakeHost();
  const hooks = await working(host);
  await status(hooks, SESSION, "idle");
  host.take();

  await status(hooks, SESSION, "busy");

  assert.deepEqual(host.last(), { state: "working", payload: { session_id: SESSION } });
});

test("dispose forgets the agent and keeps the resume command", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  await hooks.dispose!();

  // "clear" is what deletes AGENT_KIND, AGENT_MSG and AGENT_STATE and leaves
  // AGENT_RESUME alone, so `cattery save` can still read the window.
  assert.deepEqual(host.last(), { state: "clear", payload: { session_id: SESSION } });
});

// --- waiting for the user -----------------------------------------------------

function asked(hooks: Hooks, id: string, request: string): Promise<void> {
  return fire(hooks, "permission.asked", { sessionID: id, id: request, permission: "bash", patterns: [] });
}

function replied(hooks: Hooks, id: string, request: string): Promise<void> {
  return fire(hooks, "permission.replied", { sessionID: id, requestID: request, reply: "once" });
}

test("a permission request reads as blocked until the last one is answered", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  await asked(hooks, SESSION, "per_1");
  assert.deepEqual(host.last(), { state: "blocked", payload: { session_id: SESSION } });

  await asked(hooks, SESSION, "per_2");
  assert.deepEqual(host.take(), [], "still blocked, so nothing to say");

  // opencode publishes a permission.replied for every request an "always"
  // answer cascades onto, each with its own id. Counting replies rather than
  // outstanding ids would take the agent out of blocked here.
  await replied(hooks, SESSION, "per_1");
  assert.deepEqual(host.take(), [], "one request is still waiting");

  await replied(hooks, SESSION, "per_2");
  assert.deepEqual(host.last(), { state: "working", payload: { session_id: SESSION } });
});

test("a reply for a request nobody is waiting on changes nothing", async () => {
  const host = new FakeHost();
  const hooks = await working(host);
  await status(hooks, SESSION, "idle");
  host.take();

  await replied(hooks, SESSION, "per_gone");

  // A reply cannot reach an agent that is not waiting, and publishing "working"
  // here would put a running marker on a tab whose turn has ended.
  assert.deepEqual(host.take(), []);
});

test("a question blocks the same way a permission does", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  await fire(hooks, "question.asked", { sessionID: SESSION, id: "que_1", questions: [] });
  assert.deepEqual(host.last(), { state: "blocked", payload: { session_id: SESSION } });

  await fire(hooks, "question.rejected", { sessionID: SESSION, requestID: "que_1" });
  assert.deepEqual(host.last(), { state: "working", payload: { session_id: SESSION } });
});

// --- subagents ----------------------------------------------------------------

const CHILD = "ses_child";

test("a subagent turn does not end the parent's turn", async () => {
  const host = new FakeHost();
  const hooks = await working(host);
  await created(hooks, CHILD, SESSION);

  await prompt(hooks, CHILD, "read the registry");
  await status(hooks, CHILD, "idle");
  await fire(hooks, "permission.asked", { sessionID: CHILD, id: "per_1", permission: "bash", patterns: [] });

  assert.deepEqual(host.take(), [], "a child going idle would draw the done marker over a running agent");

  // And the parent still ends its own turn.
  await status(hooks, SESSION, "idle");
  assert.deepEqual(host.last(), { state: "idle", payload: { tool: "", session_id: SESSION } });
});

test("a session id never seen created is resolved once", async () => {
  const host = new FakeHost();
  const hooks = await working(host);
  host.parents.set(CHILD, SESSION);

  await prompt(hooks, CHILD, "read the registry");
  await status(hooks, CHILD, "idle");

  assert.deepEqual(host.take(), []);
  assert.equal(host.lookups, 1, "the answer is cached, so the second event asks nobody");
});

test("a server that cannot answer leaves the agent listed", async () => {
  const host = new FakeHost();
  const hooks = await load(host);
  host.take();

  // No session.created, no reachable server: the resume case with nothing to
  // go on. Reading it as top-level costs one early done marker; the other guess
  // would keep a real agent out of the picker for its whole life.
  await prompt(hooks, SESSION, "fix the picker");

  assert.deepEqual(host.last().payload["session_id"], SESSION);
});

test("a new top-level session repoints the window", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  await created(hooks, "ses_second");

  assert.deepEqual(host.last(), {
    state: "idle",
    payload: { source: "startup", tool: "", session_id: "ses_second" },
  });
});

// The plugin reads untyped event payloads, because the SDK's Event union is a
// stale artifact. A throw inside an event handler would be an unhandled
// rejection: opencode calls the hook with a bare `void`.
test("an event missing the fields the plugin reads publishes nothing", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  for (const type of ["session.status", "session.created", "permission.asked", "permission.replied"]) {
    await hooks.event!({ event: { type } } as never);
    await fire(hooks, type, { sessionID: SESSION });
  }

  assert.deepEqual(host.take(), []);
});

// Same reason: a cattery binary that is missing, or a fork that fails, must not
// reach opencode as a rejection.
test("a runner that throws does not reach the agent", async () => {
  const host = new FakeHost();
  const hooks = await working(host);
  setRunner(() => {
    throw new Error("spawnSync ENOENT");
  });

  await status(hooks, SESSION, "idle");
  await hooks.dispose!();
});

test("session.updated does not repoint the window", async () => {
  const host = new FakeHost();
  const hooks = await working(host);

  // It fires all through a turn, for a title or a token count.
  await fire(hooks, "session.updated", { sessionID: "ses_second", info: { id: "ses_second" } });

  assert.deepEqual(host.take(), []);
});

// --- the running tool ---------------------------------------------------------

type Timers = TestContext["mock"]["timers"];

// Long enough to pass the plugin's tool debounce. The tests move a mocked clock
// rather than wait, so this only has to be past it, not equal to it.
const PAST_DEBOUNCE_MS = 5_000;

function before(hooks: Hooks, callID: string, tool: string, args: unknown): Promise<void> {
  return hooks["tool.execute.before"]!({ tool, sessionID: SESSION, callID }, { args });
}

function after(hooks: Hooks, callID: string, tool: string): Promise<void> {
  return hooks["tool.execute.after"]!(
    { tool, sessionID: SESSION, callID, args: {} },
    { title: "", output: "", metadata: {} },
  );
}

/** Start a turn with the tool debounce under the test's own clock. */
async function toolTurn(timers: Timers, host: FakeHost): Promise<Hooks> {
  timers.enable({ apis: ["setTimeout", "Date"] });
  return working(host);
}

test("a running tool is published with the second it started", async (t) => {
  const host = new FakeHost();
  const hooks = await toolTurn(t.mock.timers, host);

  const startedAt = Math.floor(Date.now() / 1000);
  await before(hooks, "c1", "bash", { command: "go   test ./..." });
  assert.deepEqual(host.take(), [], "a tool worth showing runs for minutes, so nothing forks at once");
  t.mock.timers.tick(PAST_DEBOUNCE_MS);

  // The call's own start, not the moment the debounce fired: the watcher weighs
  // this against the stall threshold.
  assert.deepEqual(host.last(), {
    state: "working",
    payload: { tool: "bash: go test ./...", tool_since: startedAt, session_id: SESSION },
  });
});

test("the label names the argument that says what the tool is doing", async (t) => {
  const cases: Array<[name: string, tool: string, args: unknown, want: string]> = [
    // opencode's shell tool answers to "bash": its id and its permission key
    // both keep that word until opencode 2.0.
    ["the shell tool", "bash", { command: "sleep 900" }, "bash: sleep 900"],
    // read, write and edit take filePath, not the path pi's tools take.
    ["a file path", "read", { filePath: "/tmp/x.go" }, "read: /tmp/x.go"],
    ["a pattern", "grep", { pattern: "AGENT_", path: "/tmp" }, "grep: AGENT_"],
    ["a subagent", "task", { description: "audit the picker", prompt: "..." }, "task: audit the picker"],
    ["no known argument", "read", { offset: 3 }, "read"],
    ["no arguments at all", "read", undefined, "read"],
    // An MCP server can register a tool taking anything, and a number is not a
    // label.
    ["an argument that is not a string", "read", { filePath: 3 }, "read"],
    ["a tool nothing knows about", "mcp_search", { query: "x" }, "mcp_search"],
  ];
  const host = new FakeHost();
  t.mock.timers.enable({ apis: ["setTimeout", "Date"] });
  const hooks = await working(host);

  for (const [name, tool, args, want] of cases) {
    await before(hooks, name, tool, args);
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
    assert.equal(host.last().payload["tool"], want, name);
    await after(hooks, name, tool);
    t.mock.timers.tick(PAST_DEBOUNCE_MS);
    host.take();
  }
});

test("a label over the cap is cut", async (t) => {
  const host = new FakeHost();
  const hooks = await toolTurn(t.mock.timers, host);

  await before(hooks, "c1", "bash", { command: "x".repeat(300) });
  t.mock.timers.tick(PAST_DEBOUNCE_MS);

  const label = host.last().payload["tool"] as string;
  assert.equal(label.length, 120);
  assert.ok(label.endsWith("…"), label);
});

test("the earliest open call is the one published", async (t) => {
  const host = new FakeHost();
  const hooks = await toolTurn(t.mock.timers, host);

  await before(hooks, "c1", "bash", { command: "sleep 900" });
  t.mock.timers.tick(PAST_DEBOUNCE_MS);
  const hung = host.last().payload["tool_since"];

  // A fast read starting beside a bash hung for minutes. Publishing the newest
  // call would restamp the timestamp and the stall would never fire.
  t.mock.timers.tick(60_000);
  await before(hooks, "c2", "read", { filePath: "/tmp/x.go" });
  t.mock.timers.tick(PAST_DEBOUNCE_MS);
  assert.deepEqual(host.take(), [], "the read is not the stall candidate");

  // The hung call ends, so the read is now the earliest.
  await after(hooks, "c1", "bash");
  t.mock.timers.tick(PAST_DEBOUNCE_MS);
  const promoted = host.last();
  assert.equal(promoted.payload["tool"], "read: /tmp/x.go");
  assert.ok((promoted.payload["tool_since"] as number) > (hung as number), "the read carries its own start");

  await after(hooks, "c2", "read");
  t.mock.timers.tick(PAST_DEBOUNCE_MS);
  assert.deepEqual(host.last(), { state: "working", payload: { tool: "", session_id: SESSION } });
});

test("a call shorter than the debounce forks nothing", async (t) => {
  const host = new FakeHost();
  const hooks = await toolTurn(t.mock.timers, host);

  await before(hooks, "c1", "read", { filePath: "/tmp/x.go" });
  await after(hooks, "c1", "read");
  t.mock.timers.tick(PAST_DEBOUNCE_MS);

  assert.deepEqual(host.take(), []);
});

test("a new turn drops the last one's tool", async (t) => {
  const host = new FakeHost();
  const hooks = await toolTurn(t.mock.timers, host);
  await before(hooks, "c1", "bash", { command: "sleep 900" });
  t.mock.timers.tick(PAST_DEBOUNCE_MS);
  host.take();

  // An interrupt tears the turn down with no tool.execute.after, so a label
  // pinned with an old timestamp would read as stalled from the next turn's
  // first second.
  await prompt(hooks, SESSION, "stop that");
  assert.deepEqual(host.last().payload["tool"], "");

  t.mock.timers.tick(PAST_DEBOUNCE_MS);
  assert.deepEqual(host.take(), [], "and the pending write went with it");
});

test("a tool call for a subagent does not reach the window", async (t) => {
  const host = new FakeHost();
  const hooks = await toolTurn(t.mock.timers, host);
  await created(hooks, CHILD, SESSION);

  await hooks["tool.execute.before"]!(
    { tool: "bash", sessionID: CHILD, callID: "c1" },
    { args: { command: "sleep 900" } },
  );
  t.mock.timers.tick(PAST_DEBOUNCE_MS);

  assert.deepEqual(host.take(), []);
});

// The approval gate runs inside each tool's execute, so tool.execute.before has
// already fired when the dialog opens. Blocked has to win: the row says the
// agent is waiting, not that its call has hung.
test("a call sitting in the approval dialog reads as blocked", async (t) => {
  const host = new FakeHost();
  const hooks = await toolTurn(t.mock.timers, host);

  await before(hooks, "c1", "bash", { command: "rm -rf build" });
  await asked(hooks, SESSION, "per_1");
  t.mock.timers.tick(PAST_DEBOUNCE_MS);

  const runs = host.take();
  assert.equal(runs[0]!.state, "blocked");
  // The label still goes out, and the watcher only weighs AGENT_TOOL_SINCE
  // against the threshold while the state is working.
  assert.equal(runs[1]!.payload["tool"], "bash: rm -rf build");
  assert.equal(runs[1]!.state, "blocked");
});
