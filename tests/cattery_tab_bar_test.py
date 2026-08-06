"""Unit tests for kitty/tab_bar.py, the default tab bar `cattery setup` installs.

kitty loads `tab_bar.py` with `runpy.run_path`, which does not extend
`sys.path`, so the file adds its own directory before importing `cattery_tab`.
Both outcomes of that import have to leave a working `draw_title`: an
ImportError raised at module scope runs before `draw_title` is defined and
disables the whole tab bar, not only the agent glyph.

Each case copies `tab_bar.py` into a temporary directory and loads it from
there, because that is what decides whether `cattery_tab` is importable: the
copy that sits beside `cattery_tab.py` finds it, and the copy that sits alone
does not. `sys.modules` and `sys.path` are restored after every load, so a
successful import in one case cannot satisfy the next one from cache.

Run with `make test-python`.
"""

import importlib.util
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
KITTY_DIR = REPO_ROOT / "kitty"


class FakeFg:
    tab = "<tab>"
    red = "<red>"
    green = "<green>"
    yellow = "<yellow>"


class FakeFmt:
    fg = FakeFg()


class TabBarTest(unittest.TestCase):
    def load(self, *names):
        """Copy the named kitty files into a fresh directory and load tab_bar.py."""
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory)
        for name in ("tab_bar.py",) + names:
            shutil.copy(KITTY_DIR / name, directory / name)

        saved_path = list(sys.path)
        saved_module = sys.modules.pop("cattery_tab", None)
        self.addCleanup(self.restore, saved_path, saved_module)

        spec = importlib.util.spec_from_file_location("tab_bar_under_test", directory / "tab_bar.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def restore(self, saved_path, saved_module):
        sys.path[:] = saved_path
        sys.modules.pop("cattery_tab", None)
        if saved_module is not None:
            sys.modules["cattery_tab"] = saved_module

    def test_cattery_tab_available(self):
        module = self.load("cattery_tab.py")
        self.assertEqual(module.agent_prefix.__module__, "cattery_tab")
        self.assertTrue(callable(module.draw_title))
        # No AGENT_DISPLAY on this tab, so the glyph is empty and the title
        # renders as it would without cattery.
        self.assertEqual(module.draw_title({"fmt": None, "title": "zsh", "index": 1}), " 1: zsh")

    def test_cattery_tab_missing_still_draws_titles(self):
        module = self.load()
        self.assertEqual(module.agent_prefix({}), "")
        self.assertTrue(callable(module.draw_title))
        self.assertEqual(module.draw_title({"fmt": None, "title": "zsh", "index": 1}), " 1: zsh")

    def test_styled_tab_bar_keeps_the_tab_color(self):
        module = self.load("cattery_tab.py")
        got = module.draw_title({"fmt": FakeFmt(), "title": "zsh", "index": 2})
        self.assertEqual(got, " 2: <tab>zsh")

    def test_missing_title_and_index_do_not_raise(self):
        module = self.load("cattery_tab.py")
        self.assertEqual(module.draw_title({"fmt": None}), " : ")


if __name__ == "__main__":
    unittest.main()
