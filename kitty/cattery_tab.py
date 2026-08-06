"""
cattery_tab: the agent-state marker for a kitty tab.

kitty loads `tab_bar.py` from its config directory through `runpy.run_path`,
which does not extend `sys.path`, so a `tab_bar.py` that wants this module has
to add its own directory first:

    import os
    import sys

    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    try:
        from cattery_tab import agent_prefix
    except Exception:
        def agent_prefix(data):
            return ""

Guard that import. An ImportError at module scope runs before `draw_title` is
defined and disables the whole tab bar.

This module reads AGENT_DISPLAY and AGENT_SINCE, which cattery_watcher.py
writes. Without the watcher loaded there is no AGENT_DISPLAY, and every call
returns "".
"""

import time

# (color_name, glyph, urgency_rank). The lower rank wins, so a tab holding one
# blocked agent and three working ones shows blocked. A seen idle agent is
# missing from the table, because it draws nothing.
_AGENT_STATE_STYLE = {
    "blocked": ("red", "◆", 0),     # ◆ needs input
    "done":    ("green", "●", 1),   # ● finished, unseen
    "working": ("yellow", "●", 2),  # ● working
}

# Displays that carry an elapsed time. A finished agent has a timestamp too, and
# the tab bar leaves that to the picker, which phrases it as "… ago".
_TIMED = ("working", "blocked")


def _collect_agent_display(tab):
    """Return (display, style, window) for the tab's most urgent agent, or None."""
    if tab is None:
        return None
    best = None
    for w in tab:
        display = w.user_vars.get("AGENT_DISPLAY")
        style = _AGENT_STATE_STYLE.get(display)
        if style is None:
            continue
        if best is None or style[2] < best[1][2]:
            best = (display, style, w)
    return best


def _agent_elapsed(window):
    """Minutes since the window's last AGENT_DISPLAY change, or an empty string."""
    raw = window.user_vars.get("AGENT_SINCE")
    if not raw:
        return ""
    try:
        delta = time.time() - int(raw)
    except ValueError:
        return ""
    if delta < 60:
        return ""
    return f" {int(delta // 60)}m"


def agent_prefix(data):
    """
    Build the coloured agent-state marker for this tab, or an empty string.

    `data` is the dict kitty hands to `draw_title`. Every error is caught, so a
    failure here falls back to plain rendering instead of breaking the tab bar.
    """
    try:
        from kitty.fast_data_types import get_boss  # type: ignore
    except Exception:
        return ""
    fmt = data.get("fmt")
    tab_id = data.get("tab_id")
    if tab_id is None:
        return ""
    try:
        boss = get_boss()
        info = _collect_agent_display(boss.tab_for_id(tab_id))
        if info is None:
            return ""
        display, (color_name, glyph, _rank), window = info
        elapsed = _agent_elapsed(window) if display in _TIMED else ""
        if fmt is None:
            return f"{glyph}{elapsed} "
        return f"{getattr(fmt.fg, color_name)}{glyph}{elapsed} {fmt.fg.tab}"
    except Exception:
        return ""
