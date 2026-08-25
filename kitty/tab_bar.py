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
from functools import lru_cache


def _gitdir(gitfile):
    try:
        with open(gitfile, encoding="utf-8") as f:
            line = f.readline().strip()
    except OSError:
        return ""
    prefix = "gitdir:"
    if not line.lower().startswith(prefix):
        return ""
    path = line[len(prefix) :].strip()
    if not os.path.isabs(path):
        path = os.path.join(os.path.dirname(gitfile), path)
    return os.path.normpath(path)


@lru_cache(maxsize=256)
def _worktree_root(cwd):
    if not cwd:
        return ""
    path = os.path.abspath(cwd)
    while True:
        marker = os.path.join(path, ".git")
        if os.path.isfile(marker):
            gitdir = _gitdir(marker)
            if gitdir and os.path.isfile(os.path.join(gitdir, "commondir")):
                return path
        elif os.path.isdir(marker):
            return ""
        parent = os.path.dirname(path)
        if parent == path:
            return ""
        path = parent


def _tilde_path(path):
    home = os.path.normpath(os.path.expanduser("~"))
    try:
        if os.path.commonpath((home, path)) != home:
            return ""
    except ValueError:
        return ""
    relative = os.path.relpath(path, home)
    return "~" if relative == "." else os.path.join("~", relative)


def _abbreviate_path(path):
    parent, name = os.path.split(path)
    parts = parent.split(os.sep)
    abbreviated = []
    for part in parts:
        if part in ("", "~"):
            abbreviated.append(part)
        elif part.startswith("."):
            abbreviated.append(part[:2])
        else:
            abbreviated.append(part[:1])
    return os.sep.join((*abbreviated, name))


def _title_paths(path):
    paths = [path]
    tilde = _tilde_path(path)
    if tilde:
        paths.append(tilde)
    paths.extend(_abbreviate_path(value) for value in tuple(paths))
    return paths


def shorten_worktree_title(title, cwd):
    root = _worktree_root(cwd)
    if not root:
        return title
    paths = set(_title_paths(root))
    paths.update(_title_paths(os.path.abspath(cwd)))
    for path in sorted(paths, key=len, reverse=True):
        if path not in title:
            continue
        before, rest = title.split(path, 1)
        if rest and not rest.startswith((os.sep, ":", " -", " —", " |")):
            continue
        keep_rest = rest if rest.startswith((":", " -", " —", " |")) else ""
        return f"{before}[wt] {os.path.basename(root)}{keep_rest}"
    return title


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
    tab = data.get("tab")
    try:
        cwd = getattr(tab, "active_wd", "")
    except Exception:
        cwd = ""
    title = shorten_worktree_title(data.get("title", ""), cwd)
    index = data.get("index", "")
    if fmt is None:
        return f" {index}: {agent_prefix(data)}{title}"
    return f" {index}: {agent_prefix(data)}{fmt.fg.tab}{title}"
