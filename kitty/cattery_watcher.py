"""
cattery_watcher: kitty global watcher. It turns the per-window AGENT_STATE that
pi and Claude Code publish into a display state that also knows whether the user
has looked at the window.

Contract:
    AGENT_KIND        "pi" | "claude"                    (set by the agent)
    AGENT_STATE       "working" | "blocked" | "idle"     (set by the agent)
    AGENT_TOOL        the tool call running now          (set by the agent, pi only)
    AGENT_TOOL_SINCE  unix seconds when that call started (set by the agent)
    AGENT_DISPLAY     "working" | "stalled" | "blocked" | "done" | "idle"
                      (set here, read by cattery_tab.py and the kittens)
    AGENT_SINCE       unix seconds of the last AGENT_DISPLAY change
                      (set here, read by the Go picker)

Display derivation:
    working  -> working, or stalled when one tool call has run past
                _STALL_THRESHOLD   (seen state cleared)
    blocked  -> blocked   (seen state cleared)
    idle     -> done when the agent had been working, stalled or blocked and the
                window has not been focused since, else idle

Nothing fires while a tool hangs, so a window can sit in "working" until its
next event. One repeating timer sweeps the working windows for it.

Shared state lives on the `boss` object, so the watcher, cattery_tab.py, and the
kittens see one view with no state file and no daemon. The tab bar loads
cattery_tab.py through runpy, which gives it no shared sys.path.

    boss._agent_seen   set[int]         windows the user visited since they
                                        went idle
    boss._agent_titles dict[int, str]   OS-window titles this watcher set, so
                                        it clears only its own
    boss._agent_subs   dict[str, None]  unix datagram socket paths that asked
                                        for the transitions, in the order they
                                        registered. cattery_events.py puts them
                                        there.
    boss._agent_timer  int | None       the id of the stall sweep's timer, so a
                                        config reload does not start a second

Side effects on a transition:
    * set AGENT_DISPLAY on the window
    * mark_tab_bar_dirty() on the window's tab manager
    * set_os_window_title() with a "(N need you)" summary for that OS window
    * a notification through kitty's own notification manager on an edge into
      blocked or done, unless the window is focused
    * one JSON datagram per registered subscriber

Re-entrancy: writing AGENT_DISPLAY fires on_set_user_var again, so every key
other than AGENT_STATE, AGENT_KIND and AGENT_TOOL is ignored.
"""

import errno
import json
import socket
import time
from functools import partial
from typing import Any

from kitty.boss import Boss
from kitty.fast_data_types import get_options, set_os_window_title
from kitty.utils import log_error
from kitty.window import Window

try:
    from kitty.notifications import OnlyWhen, Urgency
except ImportError:  # kitty older than the notification manager
    OnlyWhen = None

# The key the picker sets to say the user has looked at an agent. The set of
# seen windows lives in this process and nothing outside kitty can reach it, so
# a user variable is the way in. The watcher clears it again once it has read
# it, which keeps it from surviving as stale state on the window.
_SEEN_KEY = "AGENT_SEEN"

# The key naming the tool call in flight, and the only agent kind that writes
# it. AGENT_TOOL_SINCE is deliberately not an input key: the writer sets it
# first, so reacting to it would apply the new timestamp against the previous
# label.
_TOOL_KEY = "AGENT_TOOL"
_TOOL_KIND = "pi"

# The keys the watcher reacts to. AGENT_DISPLAY is its own output, and ignoring
# it breaks the feedback loop. Other software's user variables stop here too.
_INPUT_KEYS = ("AGENT_STATE", "AGENT_KIND", _TOOL_KEY, _SEEN_KEY)

# How long one tool call has to run before a working agent reads as stalled, and
# how often the sweep looks. Ten minutes, not five: pi's subagent calls routinely
# run several minutes, so a shorter threshold would flag ordinary work. A minute
# of granularity against that threshold is close enough, so the picker, which
# reloads every second, can show stalled up to a minute before the tab does. Keep
# the threshold in step with StallThreshold in internal/agent:
# test_the_threshold_is_ten_minutes and TestStalled there both write the ages
# out, so moving one number alone fails one of the two.
_STALL_THRESHOLD = 600.0
_SWEEP_INTERVAL = 60.0

# Display states we care about for notifications and the OS-window summary.
_ATTENTION = ("blocked", "done", "stalled")

# Displays meaning "this agent has done something", which can turn into "done"
# when the agent goes idle unseen. "stalled" belongs here: without it a run
# going working -> stalled -> idle finishes with no marker and no notification,
# which is the agent you most wanted to hear about.
_WORKED = ("working", "blocked", "done", "stalled")

# The displays that are one turn seen twice. AGENT_SINCE is not restamped when
# an agent moves between them, or the tab's elapsed minutes would reset to zero
# exactly when the number starts mattering.
_TURN = ("working", "stalled")

_TITLE_TPL = {
    "blocked": "Agent needs input",
    "done": "Agent finished",
    "stalled": "Agent may be stuck",
    "working": "Agent",
    "idle": "Agent",
}

# sendto errors that mean the subscriber has gone: no socket file at the path,
# or a file nobody is bound to. ENOBUFS is not one of them. It means the
# subscriber is alive and behind, so that datagram is dropped and the
# registration stays.
_DEAD_SUBSCRIBER = (errno.ENOENT, errno.ECONNREFUSED)

# The longest title or prompt an event carries. macOS refuses a unix datagram
# over net.local.dgram.maxdgram, 2048 bytes by default, and answers EMSGSIZE,
# which reads here as "alive and behind" and drops the event with nothing said.
# Both writers already cap a prompt at 200 characters; a window title is
# whatever the program in it set, and nothing caps that.
_FIELD_LIMIT = 200

# The socket every event goes out on, made on the first send. It is never bound
# and never read: sendto names the receiver and a subscriber does not answer.
_sender: socket.socket | None = None


def _ensure_state(boss: Boss) -> None:
    """Idempotently initialize the shared blackboard on `boss`."""
    if not hasattr(boss, "_agent_seen"):
        boss._agent_seen = set()
    if not hasattr(boss, "_agent_titles"):
        boss._agent_titles = {}
    if not hasattr(boss, "_agent_subs"):
        boss._agent_subs = {}
    if not hasattr(boss, "_agent_timer"):
        boss._agent_timer = None


def _derive_display(
    state: str | None,
    window_id: int,
    seen: set,
    is_focused: bool,
    prev: str | None,
    tool_since: float | None,
) -> str | None:
    """
    Compute AGENT_DISPLAY from AGENT_STATE.

    Returns None for a window with no AGENT_STATE, which never opted into the
    contract.

    An agent that goes idle while the user watches the window is marked seen at
    once, so the "done" marker never appears. There is nothing to alert.

    `prev` is the window's current AGENT_DISPLAY. "done" means "finished
    something you have not looked at", so only a state that did work reaches it.
    Agents announce `idle` when they start, as pi does on session_start and
    Claude does on SessionStart, and a start after a clean exit reads as idle:
    the agent's own clear dropped AGENT_STATE, which drops AGENT_DISPLAY with
    it. A window whose agent was killed keeps AGENT_DISPLAY="working", so the
    next session's opening idle reads as "done" once while the window is
    unfocused. Nothing outside can prevent that, because AGENT_DISPLAY is this
    module's own output.

    `tool_since` is when the tool call in flight started, and None when the
    agent publishes none. An agent without one never reads as stalled, which is
    what keeps Claude agents out of that state with no special case.
    """
    if state in ("working", "blocked"):
        # Activity beats "seen". An agent that went back to work should remind
        # the user again when it next finishes.
        seen.discard(window_id)
        if state == "working" and tool_since is not None and time.time() - tool_since >= _STALL_THRESHOLD:
            return "stalled"
        return state
    if state == "idle":
        if is_focused:
            seen.add(window_id)
        if window_id in seen or prev not in _WORKED:
            return "idle"
        return "done"
    return None


def _notify(boss: Boss, window: Window, kind: str, display: str) -> None:
    """Fire an edge-triggered notification through kitty's own manager."""
    if OnlyWhen is None:
        return
    manager = getattr(boss, "notification_manager", None)
    if manager is None:
        return
    label = (kind or "agent").strip() or "agent"
    try:
        cmd = manager.create_notification_cmd()
        cmd.title = f"{_TITLE_TPL.get(display, 'Agent')} ({label})"
        # A window title is whatever the program in it last wrote. kitty
        # sanitizes both title and body itself, in finalise().
        cmd.body = window.title or label
        # One identifier per window and state, so a repeat of the same state
        # replaces its own banner and blocked and done coexist.
        cmd.identifier = f"cattery-{window.id}-{display}"
        cmd.application_name = "cattery"
        cmd.notification_types = (f"agent-{display}",)
        # For Linux. kitty's cocoa switch on urgency has no break, so on macOS
        # every notification arrives at the same level whatever is asked for.
        cmd.urgency = Urgency.Critical if display == "blocked" else Urgency.Normal
        # macOS cannot pick a sound per state, only sound or no sound.
        cmd.sound_name = "silent"
        # Already the default, which does not gate on focus either. Said out
        # loud because _apply has decided this window is unfocused, and a
        # default that began consulting kitty's own idea of focus would ask
        # that question a second time.
        cmd.only_when = OnlyWhen.always
        # No Action.focus: _activated focuses instead, so pressing the button
        # does not also drag the user to the agent window.
        cmd.actions = frozenset()
        # No button when there is nothing to launch, rather than one that
        # silently does nothing.
        cmd.buttons = ("Open picker",) if _picker_binary() else ()
        cmd.on_activation = partial(_activated, boss, window.id)
        manager.notify_with_command(cmd, window.id)
    except Exception as err:
        # kitty's notification API is internal and can change shape. The tab
        # marker still shows the state, so this is the only trace left of a
        # notification path that has stopped working.
        log_error(f"cattery: notification failed: {err}")


def _activated(boss: Boss, window_id: int, cmd: Any, button: int) -> None:
    """Handle a press on the banner. Button 0 is the body, 1 the first button."""
    if button == 1:
        _open_picker(boss)
        return
    # The window may have closed between the notification and the press.
    target = boss.window_id_map.get(window_id)
    if target is not None:
        # on_focus_change marks it seen, which drops the done marker.
        #
        # The token comes from the command because kitty's own focus path,
        # which is the only other thing that passes one, is off: `actions` is
        # empty. A Wayland compositor with focus-stealing prevention discards a
        # raise that carries no token.
        boss.set_active_window(
            target,
            switch_os_window_if_needed=True,
            activation_token=getattr(cmd, "activation_token", ""),
        )


def _open_picker(boss: Boss) -> None:
    """Launch the picker, which setup names in the managed kitty.conf block."""
    binary = _picker_binary()
    if not binary:
        # An install predating the env line. There is nothing to launch.
        return
    boss.launch("--type=overlay", "--copy-colors", binary)


def _picker_binary() -> str:
    """Where the picker is, as setup wrote it into the managed kitty.conf block.

    The watcher is a static installed file with no way to know where the binary
    lives, and it cannot look it up on PATH: a kitty started from the Dock has
    launchd's, which has no Homebrew.
    """
    return get_options().env.get("CATTERY_BIN", "")


def _publish(boss: Boss, window: Window, frm: str | None, to: str) -> None:
    """Send one transition to every registered subscriber.

    The event is one JSON object per datagram, with no framing:

        {"ts":1755302096,"window":363,"kind":"pi","from":"working",
         "to":"blocked","title":"~/projects/notes","cwd":"/Users/x/notes",
         "msg":"fix the picker","focused":false}

    `to` is a display state, or "cleared" when the agent dropped its state and
    "closed" when the window went away. `frm` is the previous display, null the
    first time a window is seen.

    This runs on kitty's own thread, so it must neither block nor raise. The
    socket is non-blocking, and the body is guarded the way _write_os_title is:
    a subscriber cannot cost the tab marker or the notification.

    Nothing at all happens with an empty registry, which is every cattery
    nobody has subscribed to.
    """
    try:
        subs = boss._agent_subs
        if not subs:
            return
        event = json.dumps(
            {
                "ts": int(time.time()),
                "window": window.id,
                "kind": window.user_vars.get("AGENT_KIND", ""),
                "from": frm,
                "to": to,
                "title": _clip(window.title or ""),
                # current_cwd is the field `kitten @ ls` reports, so an event
                # and a picker row name the same directory. cwd_of_child reads
                # the foreground process instead, which for a sandboxed agent
                # is its log reader.
                "cwd": getattr(getattr(window, "child", None), "current_cwd", "") or "",
                "msg": _clip(window.user_vars.get("AGENT_MSG", "")),
                "focused": bool(window.is_focused),
            },
            # One line with no padding, and the text as the UTF-8 JSON is
            # defined to be. The default would spend six bytes on every
            # non-ASCII character, which a datagram has no room for.
            ensure_ascii=False,
            separators=(",", ":"),
        ).encode()
        sender = _sender_socket()
        for path in list(subs):
            try:
                sender.sendto(event, path)
            except OSError as err:
                if err.errno in _DEAD_SUBSCRIBER:
                    subs.pop(path, None)
    except Exception:
        return


def _clip(text: str) -> str:
    """Cut a field to the length an event has room for."""
    if len(text) <= _FIELD_LIMIT:
        return text
    return text[: _FIELD_LIMIT - 1] + "…"


def _sender_socket() -> socket.socket:
    global _sender
    if _sender is None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
        sock.setblocking(False)
        _sender = sock
    return _sender


def _update_os_title(boss: Boss, os_window_id: int, closing_window_id: int | None = None) -> None:
    """Prefix the OS-window title with `(N need you)`, or restore the default.

    set_os_window_title("") restores kitty's default, which tracks the active
    window's title. A count above zero overrides that title and keeps the active
    window's own title after the prefix, so the macOS Dock and the ⌘-Tab
    switcher still name the window the user is looking at.

    The embedded title goes stale when the active window changes or when the
    shell retitles it, so this also runs on focus and title changes. The watcher
    remembers the last value it wrote per OS window. It skips an identical
    write, and it restores the default only for a title it set itself.

    `closing_window_id` drops one window from the count. kitty calls the close
    watcher while the window is still in its tab, so counting it would leave
    `(1 need you)` behind for the agent that just went away.

    The remembered value is stored only after a successful write, so a failed
    write is retried on the next call.
    """
    _ensure_state(boss)
    tm = boss.os_window_map.get(os_window_id)
    if tm is None:
        # The OS window is gone. Drop the bookkeeping with it.
        boss._agent_titles.pop(os_window_id, None)
        return
    count = 0
    for tab in tm:
        for w in tab:
            if w.id == closing_window_id:
                continue
            if w.user_vars.get("AGENT_DISPLAY") in _ATTENTION:
                count += 1
    if count <= 0:
        if boss._agent_titles.get(os_window_id) is None:
            return
        if _write_os_title(os_window_id, ""):
            boss._agent_titles.pop(os_window_id, None)
        return
    active = tm.active_window
    suffix = (active.title or "") if active is not None else ""
    title = f"({count} need you) {suffix}".rstrip()
    if boss._agent_titles.get(os_window_id) == title:
        return
    if _write_os_title(os_window_id, title):
        boss._agent_titles[os_window_id] = title


def _write_os_title(os_window_id: int, title: str) -> bool:
    """Set an OS-window title, reporting whether it worked.

    set_os_window_title comes from a C extension, which must never break the
    watcher.
    """
    try:
        set_os_window_title(os_window_id, title)
    except Exception:
        return False
    return True


def _redraw(boss: Boss, window: Window) -> None:
    tm = boss.os_window_map.get(window.os_window_id)
    if tm is not None:
        tm.mark_tab_bar_dirty()


def _tool_since(window: Window) -> float | None:
    """When the tool call this window is running started, or None.

    Only pi publishes the pair, and only pi clears it. A window outlives its
    agents and `cattery state clear` drops AGENT_STATE, AGENT_KIND and AGENT_MSG
    and nothing else, so a pi killed mid-call leaves its label standing: without
    the kind test the Claude started in that window would go stalled, and
    notify, inside its first second.
    """
    if window.user_vars.get("AGENT_KIND") != _TOOL_KIND:
        return None
    if not window.user_vars.get(_TOOL_KEY):
        return None
    try:
        secs = float(window.user_vars.get("AGENT_TOOL_SINCE"))
    except (TypeError, ValueError):
        return None
    # A zero is not a timestamp either, and would read as 1970.
    return secs if secs > 0 else None


def _apply(boss: Boss, window: Window) -> None:
    """Recompute AGENT_DISPLAY for a window and fan out side-effects."""
    _ensure_state(boss)
    state = window.user_vars.get("AGENT_STATE")
    kind = window.user_vars.get("AGENT_KIND", "")
    prev = window.user_vars.get("AGENT_DISPLAY")
    display = _derive_display(state, window.id, boss._agent_seen, bool(window.is_focused), prev, _tool_since(window))

    if display is None:
        # The agent cleared its state, or never had one. Drop this watcher's
        # output for the window.
        if prev is not None:
            window.set_user_var("AGENT_DISPLAY", None)
            window.set_user_var("AGENT_SINCE", None)
        _redraw(boss, window)
        _update_os_title(boss, window.os_window_id)
        if prev is not None:
            _publish(boss, window, prev, "cleared")
        return

    changed = display != prev
    if changed:
        # set_user_var fires on_set_user_var again, but the guard there ignores
        # AGENT_DISPLAY and AGENT_SINCE, so this does not recurse.
        window.set_user_var("AGENT_DISPLAY", display)
        # Wall-clock seconds, read by the tab bar and the picker. A _TURN pair
        # keeps the stamp it has.
        if not (prev in _TURN and display in _TURN):
            window.set_user_var("AGENT_SINCE", str(int(time.time())))
        _redraw(boss, window)
        _update_os_title(boss, window.os_window_id)

        if display in _ATTENTION and not window.is_focused:
            _notify(boss, window, kind, display)

        _publish(boss, window, prev, display)


def _sweep(boss: Boss) -> None:
    """Re-derive the display of every window whose agent is working.

    A hung tool call fires no event at all, so without this a window sits in
    "working" until something else recomputes it.

    Filtered to AGENT_STATE == "working" on purpose. _apply's "no display"
    branch marks the tab bar dirty and rewrites the OS-window title
    unconditionally, so an unfiltered sweep would do both for every window in
    the process, once a minute, forever.

    A timer callback that raises takes the timer with it, so nothing escapes.
    The guard is per window: kitty can tear one down between the snapshot and
    the _apply, and one raising window must not skip every window behind it in
    the iteration order, on this sweep and on all the ones after.
    """
    for window in list(boss.window_id_map.values()):
        try:
            if window.user_vars.get("AGENT_STATE") == "working":
                _apply(boss, window)
        except Exception:
            continue


def _start_sweep(boss: Boss) -> None:
    """Arm the one repeating timer behind the "stalled" display.

    add_timer is exported by kitty.fast_data_types on 0.48.1 but appears in no
    documentation page, so the import is guarded the way _write_os_title guards
    set_os_window_title. Without it the tab marker reaches stalled only when
    some other event recomputes the window; the picker derives the same rule for
    itself, so its rows are unaffected either way.

    The id lives on `boss`, never in a module global: a config reload
    re-executes this module against the same boss, and a module global would
    leak one timer per reload.
    """
    _ensure_state(boss)
    if boss._agent_timer is not None:
        return
    try:
        from kitty.fast_data_types import add_timer  # type: ignore
    except ImportError:
        return
    try:
        boss._agent_timer = add_timer(lambda *args: _sweep(boss), _SWEEP_INTERVAL, True)
    except Exception:
        boss._agent_timer = None


# --- watcher entry points (called by kitty) ----------------------------------


def on_load(boss: Boss, data: dict[str, Any]) -> None:
    # A global watcher, so this runs once per kitty process.
    _ensure_state(boss)
    _start_sweep(boss)


def on_set_user_var(boss: Boss, window: Window, data: dict[str, Any]) -> None:
    key = data.get("key")
    if key not in _INPUT_KEYS:
        # AGENT_DISPLAY, written here, and every unrelated user variable.
        return
    if key == _SEEN_KEY:
        _ensure_state(boss)
        if not window.user_vars.get(_SEEN_KEY):
            # The clearing write below arrives here as well.
            return
        boss._agent_seen.add(window.id)
        # Setting it again is what the next mark has to be able to do, and a
        # variable left behind would also travel into a session snapshot.
        window.set_user_var(_SEEN_KEY, None)
    _apply(boss, window)


def on_focus_change(boss: Boss, window: Window, data: dict[str, Any]) -> None:
    _ensure_state(boss)
    if not data.get("focused"):
        return
    if window.user_vars.get("AGENT_KIND") is not None or window.user_vars.get("AGENT_STATE") is not None:
        boss._agent_seen.add(window.id)
        # Recompute, so "done" drops to "idle" now that the user has looked.
        _apply(boss, window)
    # This runs for non-agent windows too. The title prefix quotes the active
    # window, so any focus change can leave it describing the wrong one.
    _update_os_title(boss, window.os_window_id)


def on_title_change(boss: Boss, window: Window, data: dict[str, Any]) -> None:
    # The same staleness from another cause: the active window kept focus and
    # retitled itself, which a shell prompt does on every command.
    _update_os_title(boss, window.os_window_id)


def on_close(boss: Boss, window: Window, data: dict[str, Any]) -> None:
    _ensure_state(boss)
    boss._agent_seen.discard(window.id)
    _redraw(boss, window)
    _update_os_title(boss, window.os_window_id, closing_window_id=window.id)
    prev = window.user_vars.get("AGENT_DISPLAY")
    if prev is not None:
        # A window that never carried a display was never an agent, and the
        # subscriber has nothing to close out.
        _publish(boss, window, prev, "closed")
