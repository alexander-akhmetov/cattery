"""
cattery_watcher: kitty global watcher that turns the per-window AGENT_STATE user
variable, which pi and Claude Code set, into a display state that also knows
whether the user has looked at the window.

Contract:
    AGENT_KIND     "pi" | "claude"                    (set by the agent)
    AGENT_STATE    "working" | "blocked" | "idle"     (set by the agent)
    AGENT_DISPLAY  "working" | "blocked" | "done" | "idle"
                   (written by this watcher; consumed by cattery_tab.py and the
                   picker/jump kittens)
    AGENT_SINCE    unix seconds of the last AGENT_DISPLAY transition
                   (written by this watcher; consumed by the Go picker)

Display derivation:
    working  -> working   (seen state cleared)
    blocked  -> blocked   (seen state cleared)
    idle     -> done if the agent had been working or blocked and the window
                has not been focused since, otherwise idle (no glyph)

Shared state lives directly on the `boss` object so that the watcher,
cattery_tab.py (which the tab bar loads through runpy and so has no shared
sys.path), and the custom kittens can all see the same view without a state
file or a daemon:

    boss._agent_seen   set[int]              ids of windows the user has
                                             visited since they went idle
    boss._agent_titles dict[int, str]        OS-window titles this watcher
                                             set, so it only clears its own

Side effects on transition:
    * `window.set_user_var("AGENT_DISPLAY", display)` so the tab bar and the
      kittens can read it without going through this module.
    * `mark_tab_bar_dirty()` on the window's tab manager.
    * `set_os_window_title()` updated with a "(N need you)" summary across
      that OS window.
    * `terminal-notifier` on edges into blocked/done, suppressed when the
      window is focused.

Re-entrancy: writing AGENT_DISPLAY itself fires on_set_user_var, so we ignore
any key that is not AGENT_STATE or AGENT_KIND.
"""

import subprocess
import time
from typing import Any

from kitty.boss import Boss
from kitty.fast_data_types import set_os_window_title
from kitty.window import Window

# Keys we react to. AGENT_DISPLAY is our own output; ignoring everything else
# (including unrelated user vars from other software) keeps the watcher cheap
# and avoids feedback loops.
_INPUT_KEYS = ("AGENT_STATE", "AGENT_KIND")

# Display states we care about for notifications and the OS-window summary.
_ATTENTION = ("blocked", "done")

# Displays that count as "this agent has done something", and so can turn into
# "done" when the agent goes idle unseen.
_WORKED = ("working", "blocked", "done")

# Sounds are distinct per state so the user can tell them apart by ear
# without looking at the screen. terminal-notifier maps these to macOS
# system sounds.
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

    Returns None when the window has no AGENT_STATE, so we don't claim
    windows that never opted into the contract.

    When the agent flips to `idle` while the user is already looking at the
    window, we mark it seen immediately so the "done" glyph never appears.
    The user is already watching, there's nothing to alert them to.

    `prev` is the window's current AGENT_DISPLAY. "done" means "finished
    something you haven't looked at", so it is only reachable from a state
    that did work. Agents announce `idle` when they start (pi does this on
    session_start), and a session launched in an unfocused window must not
    claim to have finished before it ran.
    """
    if state in ("working", "blocked"):
        # Activity beats "seen": if the agent went back to work, the user
        # should be reminded again when it next finishes.
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
    # -group keeps notifications per (window, state) from stacking forever in
    # Notification Center; activating one focuses kitty (default behaviour).
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
        # terminal-notifier missing or blocked by a sandbox: stay silent. The
        # tab bar glyph still shows the state.
        pass


def _update_os_title(boss: Boss, os_window_id: int, closing_window_id: int | None = None) -> None:
    """Prefix the OS-window title with `(N need you)`, or restore default.

    set_os_window_title("") restores kitty's default behaviour of tracking the
    active window's title. With a count above zero we override that title, and
    keep the active window's own title after the prefix so the macOS Dock and
    the ⌘-Tab switcher still name the window the user is looking at.

    The embedded title goes stale on its own, either because the active window
    changed or because the shell retitled it, so this runs on focus and title
    changes too. The watcher remembers the last value it wrote per OS window:
    it skips an identical write, and it restores the default only for a title
    it set itself, leaving titles owned by anything else alone.

    `closing_window_id` drops one window from the count. kitty calls the close
    watcher while the window is still in its tab and removes it afterwards, so
    counting it would leave `(1 need you)` behind for the agent that just went
    away.

    The remembered value is stored only after the write succeeds, so a failed
    write is retried on the next call instead of being recorded as done.
    """
    _ensure_state(boss)
    tm = boss.os_window_map.get(os_window_id)
    if tm is None:
        # The OS window is gone; drop the bookkeeping with it.
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
    """Set an OS-window title, reporting whether it took.

    set_os_window_title comes from a C extension; never let it break the
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
        # Agent cleared its state (or never had one). Drop our own output for
        # this window.
        if prev is not None:
            window.set_user_var("AGENT_DISPLAY", None)
            window.set_user_var("AGENT_SINCE", None)
        _redraw(boss, window)
        _update_os_title(boss, window.os_window_id)
        return

    changed = display != prev
    if changed:
        # set_user_var re-fires on_set_user_var, but our guard at the top of
        # on_set_user_var ignores AGENT_DISPLAY/AGENT_SINCE, so this won't recurse.
        window.set_user_var("AGENT_DISPLAY", display)
        # Wall-clock seconds, read by both the tab bar and the picker.
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
        # AGENT_DISPLAY (our own write) and every unrelated user var stop here.
        return
    _apply(boss, window)


def on_focus_change(boss: Boss, window: Window, data: dict[str, Any]) -> None:
    _ensure_state(boss)
    if not data.get("focused"):
        return
    if window.user_vars.get("AGENT_KIND") is not None or window.user_vars.get("AGENT_STATE") is not None:
        boss._agent_seen.add(window.id)
        # Recompute so "done" downgrades to "idle" now that the user has looked.
        _apply(boss, window)
    # Runs for non-agent windows too: the title prefix quotes whichever window
    # is active, so any focus change can leave it describing the wrong one.
    _update_os_title(boss, window.os_window_id)


def on_title_change(boss: Boss, window: Window, data: dict[str, Any]) -> None:
    # Same staleness, other cause: the active window kept focus but retitled
    # itself (a shell prompt does this on every command).
    _update_os_title(boss, window.os_window_id)


def on_close(boss: Boss, window: Window, data: dict[str, Any]) -> None:
    _ensure_state(boss)
    boss._agent_seen.discard(window.id)
    _redraw(boss, window)
    _update_os_title(boss, window.os_window_id, closing_window_id=window.id)
