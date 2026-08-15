"""
cattery_watcher: kitty global watcher. It turns the per-window AGENT_STATE that
pi and Claude Code publish into a display state that also knows whether the user
has looked at the window.

Contract:
    AGENT_KIND     "pi" | "claude"                    (set by the agent)
    AGENT_STATE    "working" | "blocked" | "idle"     (set by the agent)
    AGENT_DISPLAY  "working" | "blocked" | "done" | "idle"
                   (set here, read by cattery_tab.py and the kittens)
    AGENT_SINCE    unix seconds of the last AGENT_DISPLAY change
                   (set here, read by the Go picker)

Display derivation:
    working  -> working   (seen state cleared)
    blocked  -> blocked   (seen state cleared)
    idle     -> done when the agent had been working or blocked and the window
                has not been focused since, else idle

Shared state lives on the `boss` object, so the watcher, cattery_tab.py, and the
kittens see one view with no state file and no daemon. The tab bar loads
cattery_tab.py through runpy, which gives it no shared sys.path.

    boss._agent_seen   set[int]         windows the user visited since they
                                        went idle
    boss._agent_titles dict[int, str]   OS-window titles this watcher set, so
                                        it clears only its own

Side effects on a transition:
    * set AGENT_DISPLAY on the window
    * mark_tab_bar_dirty() on the window's tab manager
    * set_os_window_title() with a "(N need you)" summary for that OS window
    * terminal-notifier on an edge into blocked or done, unless the window is
      focused

Re-entrancy: writing AGENT_DISPLAY fires on_set_user_var again, so every key
other than AGENT_STATE and AGENT_KIND is ignored.
"""

import subprocess
import time
from typing import Any

from kitty.boss import Boss
from kitty.fast_data_types import set_os_window_title
from kitty.window import Window

# The key the picker sets to say the user has looked at an agent. The set of
# seen windows lives in this process and nothing outside kitty can reach it, so
# a user variable is the way in. The watcher clears it again once it has read
# it, which keeps it from surviving as stale state on the window.
_SEEN_KEY = "AGENT_SEEN"

# The keys the watcher reacts to. AGENT_DISPLAY is its own output, and ignoring
# it breaks the feedback loop. Other software's user variables stop here too.
_INPUT_KEYS = ("AGENT_STATE", "AGENT_KIND", _SEEN_KEY)

# Display states we care about for notifications and the OS-window summary.
_ATTENTION = ("blocked", "done")

# Displays meaning "this agent has done something", which can turn into "done"
# when the agent goes idle unseen.
_WORKED = ("working", "blocked", "done")

# One sound per state, so the user can tell them apart without looking.
# terminal-notifier maps these names to macOS system sounds.
_SOUND = {
    "blocked": "Funk",
    "done": "Glass",
}

_TITLE_TPL = {
    "blocked": "Agent needs input",
    "done": "Agent finished",
    "working": "Agent",
    "idle": "Agent",
}


def _ensure_state(boss: Boss) -> None:
    """Idempotently initialize the shared blackboard on `boss`."""
    if not hasattr(boss, "_agent_seen"):
        boss._agent_seen = set()
    if not hasattr(boss, "_agent_titles"):
        boss._agent_titles = {}


def _derive_display(
    state: str | None,
    window_id: int,
    seen: set,
    is_focused: bool,
    prev: str | None,
) -> str | None:
    """
    Compute AGENT_DISPLAY from AGENT_STATE.

    Returns None for a window with no AGENT_STATE, which never opted into the
    contract.

    An agent that goes idle while the user watches the window is marked seen at
    once, so the "done" marker never appears. There is nothing to alert.

    `prev` is the window's current AGENT_DISPLAY. "done" means "finished
    something you have not looked at", so only a state that did work reaches it.
    Agents announce `idle` when they start, as pi does on session_start, and a
    session started in an unfocused window must not claim to have finished.
    """
    if state in ("working", "blocked"):
        # Activity beats "seen". An agent that went back to work should remind
        # the user again when it next finishes.
        seen.discard(window_id)
        return state
    if state == "idle":
        if is_focused:
            seen.add(window_id)
        if window_id in seen or prev not in _WORKED:
            return "idle"
        return "done"
    return None


def _notify(window: Window, kind: str, display: str) -> None:
    """Fire an edge-triggered desktop notification via terminal-notifier."""
    title = _TITLE_TPL.get(display, "Agent")
    label = (kind or "agent").strip() or "agent"
    title_text = window.title or label
    # -group keeps one notification per window and state, instead of a stack in
    # Notification Center. Activating one focuses kitty, which is the default.
    try:
        subprocess.Popen(
            [
                "terminal-notifier",
                "-title",
                f"{title} ({label})",
                "-message",
                title_text,
                "-sound",
                _SOUND.get(display, "default"),
                "-group",
                f"kitty-agent-{window.id}-{display}",
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            close_fds=True,
        )
    except (OSError, FileNotFoundError):
        # terminal-notifier is missing or blocked by a sandbox. Stay silent;
        # the tab marker still shows the state.
        pass


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


def _apply(boss: Boss, window: Window) -> None:
    """Recompute AGENT_DISPLAY for a window and fan out side-effects."""
    _ensure_state(boss)
    state = window.user_vars.get("AGENT_STATE")
    kind = window.user_vars.get("AGENT_KIND", "")
    prev = window.user_vars.get("AGENT_DISPLAY")
    display = _derive_display(state, window.id, boss._agent_seen, bool(window.is_focused), prev)

    if display is None:
        # The agent cleared its state, or never had one. Drop this watcher's
        # output for the window.
        if prev is not None:
            window.set_user_var("AGENT_DISPLAY", None)
            window.set_user_var("AGENT_SINCE", None)
        _redraw(boss, window)
        _update_os_title(boss, window.os_window_id)
        return

    changed = display != prev
    if changed:
        # set_user_var fires on_set_user_var again, but the guard there ignores
        # AGENT_DISPLAY and AGENT_SINCE, so this does not recurse.
        window.set_user_var("AGENT_DISPLAY", display)
        # Wall-clock seconds, read by the tab bar and the picker.
        window.set_user_var("AGENT_SINCE", str(int(time.time())))
        _redraw(boss, window)
        _update_os_title(boss, window.os_window_id)

        if display in _ATTENTION and not window.is_focused:
            _notify(window, kind, display)


# --- watcher entry points (called by kitty) ----------------------------------


def on_load(boss: Boss, data: dict[str, Any]) -> None:
    _ensure_state(boss)


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
