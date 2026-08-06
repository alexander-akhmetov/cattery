"""Unit tests for kitty/cattery_tab.py, the tab-bar glyph module.

`agent_prefix` imports `get_boss` from kitty at call time, so the stub is
installed before every test rather than once at import: the watcher tests
replace the same `kitty.fast_data_types` module.

Run with `make test-python`.
"""

import importlib.util
import sys
import time
import types
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent


def _load_tab_module():
    path = REPO_ROOT / "kitty" / "cattery_tab.py"
    spec = importlib.util.spec_from_file_location("cattery_tab_under_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


tab = _load_tab_module()


class FakeWindow:
    def __init__(self, window_id=1, display=None, since=None):
        self.id = window_id
        self.user_vars = {}
        if display is not None:
            self.user_vars["AGENT_DISPLAY"] = display
        if since is not None:
            self.user_vars["AGENT_SINCE"] = since


class FakeFg:
    """Stand-in for kitty's color accessor: returns a marker per color name."""

    tab = "<tab>"
    red = "<red>"
    green = "<green>"
    yellow = "<yellow>"


class FakeFmt:
    fg = FakeFg()


def install_boss(tab_windows):
    """Point the stubbed kitty at one tab holding these windows."""
    boss = types.SimpleNamespace(tab_for_id=lambda _id: tab_windows)
    kitty = sys.modules.get("kitty") or types.ModuleType("kitty")
    kitty.__path__ = []
    fast = sys.modules.get("kitty.fast_data_types") or types.ModuleType("kitty.fast_data_types")
    fast.get_boss = lambda: boss
    sys.modules["kitty"] = kitty
    sys.modules["kitty.fast_data_types"] = fast


class AgentPrefixTest(unittest.TestCase):
    def test_glyph_for_tab(self):
        now = int(time.time())
        cases = [
            # name, windows, want
            ("no agent window", [FakeWindow(1)], ""),
            ("seen idle draws nothing", [FakeWindow(1, "idle", str(now))], ""),
            ("working", [FakeWindow(1, "working", str(now))], "<yellow>● <tab>"),
            ("blocked wins over working", [FakeWindow(1, "working"), FakeWindow(2, "blocked")], "<red>◆ <tab>"),
            ("done wins over working", [FakeWindow(1, "working"), FakeWindow(2, "done")], "<green>● <tab>"),
            # Only a live agent shows minutes; a finished one is the picker's job.
            ("working carries elapsed minutes", [FakeWindow(1, "working", str(now - 185))], "<yellow>● 3m <tab>"),
            ("done carries no elapsed", [FakeWindow(1, "done", str(now - 185))], "<green>● <tab>"),
            ("under a minute stays bare", [FakeWindow(1, "blocked", str(now - 5))], "<red>◆ <tab>"),
            ("unparsable timestamp is dropped", [FakeWindow(1, "working", "not-a-number")], "<yellow>● <tab>"),
        ]
        for name, windows, want in cases:
            with self.subTest(name):
                install_boss(windows)
                got = tab.agent_prefix({"fmt": FakeFmt(), "tab_id": 1})
                self.assertEqual(got, want)

    def test_unstyled_tab_bar_gets_the_glyph_without_colors(self):
        install_boss([FakeWindow(1, "blocked")])
        self.assertEqual(tab.agent_prefix({"fmt": None, "tab_id": 1}), "◆ ")

    def test_missing_tab_id_returns_empty(self):
        install_boss([FakeWindow(1, "blocked")])
        self.assertEqual(tab.agent_prefix({"fmt": FakeFmt()}), "")

    def test_a_raising_boss_falls_back_to_plain_rendering(self):
        install_boss([FakeWindow(1, "blocked")])
        sys.modules["kitty.fast_data_types"].get_boss = lambda: (_ for _ in ()).throw(RuntimeError("no boss"))
        self.assertEqual(tab.agent_prefix({"fmt": FakeFmt(), "tab_id": 1}), "")


if __name__ == "__main__":
    unittest.main()
