"""
tab_bar.py: kitty's tab title renderer, with the cattery agent marker in front.

`cattery setup` writes this file into the kitty config directory only when no
tab_bar.py is there already, because an existing tab bar belongs to its author.
Adding the marker to one takes two changes: the guarded import below, and one
`agent_prefix(data)` call inside `draw_title`.

kitty loads this file with `runpy.run_path`, which does not extend `sys.path`,
so the file adds its own directory before importing `cattery_tab`. Guard that
import: an ImportError at module scope runs before `draw_title` is defined and
disables the whole tab bar.
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
try:
    from cattery_tab import agent_prefix
except Exception:

    def agent_prefix(data):
        return ""


def draw_title(data):
    # kitty prepends the bell and activity symbols when the title template
    # ("{custom}") does not contain them, so do not render them here.
    fmt = data.get("fmt")
    title = data.get("title", "")
    index = data.get("index", "")
    if fmt is None:
        return f" {index}: {agent_prefix(data)}{title}"
    return f" {index}: {agent_prefix(data)}{fmt.fg.tab}{title}"
