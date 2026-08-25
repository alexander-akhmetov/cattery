"""Unit tests for kitty/tab_bar.py, the default tab bar `cattery setup` installs.

kitty loads `tab_bar.py` with `runpy.run_path`, which does not extend
`sys.path`, so the file adds its own directory before importing `cattery_tab`.
Both outcomes of that import have to leave a working `draw_title`. An
ImportError at module scope runs before `draw_title` is defined and disables the
whole tab bar.

Each case copies `tab_bar.py` into a temporary directory and loads it there,
because the directory decides whether `cattery_tab` is importable: the copy
beside `cattery_tab.py` finds it, and the copy alone does not. `sys.modules` and
`sys.path` are restored after every load, so a successful import in one case
cannot satisfy the next one from cache.

Run with `make test-python`.
"""

import importlib.util
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parent.parent
KITTY_DIR = REPO_ROOT / "kitty"


class FakeFg:
    tab = "<tab>"
    red = "<red>"
    green = "<green>"
    yellow = "<yellow>"


class FakeFmt:
    fg = FakeFg()


class FakeTab:
    def __init__(self, active_wd):
        self.active_wd = active_wd


class TabBarTest(unittest.TestCase):
    def load(self, *names):
        """Copy the named kitty files into a fresh directory, load tab_bar.py."""
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
        # No AGENT_DISPLAY on this tab, so the marker is empty and the title
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

    def test_worktree_titles_are_shortened(self):
        module = self.load("cattery_tab.py")
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory)
        root = directory / "anywhere" / "project-feature"
        nested = root / "src" / "client"
        gitdir = directory / "repo.git" / "worktrees" / "project-feature"
        nested.mkdir(parents=True)
        gitdir.mkdir(parents=True)
        (root / ".git").write_text(f"gitdir: {gitdir}\n")
        (gitdir / "commondir").write_text("../..\n")

        cases = (
            (str(root), str(root), "[wt] project-feature"),
            (str(root), module._abbreviate_path(str(root)), "[wt] project-feature"),
            (str(nested), module._abbreviate_path(str(nested)), "[wt] project-feature"),
            (str(root), f"nvim {root}: src/client.py", "nvim [wt] project-feature: src/client.py"),
        )
        for cwd, title, want in cases:
            with self.subTest(title=title):
                data = {"fmt": None, "title": title, "index": 1, "tab": FakeTab(cwd)}
                self.assertEqual(module.draw_title(data), f" 1: {want}")

        with mock.patch.object(module.os.path, "expanduser", return_value=str(directory)):
            title = "~/anywhere/project-feature"
            data = {"fmt": None, "title": title, "index": 1, "tab": FakeTab(str(root))}
            self.assertEqual(module.draw_title(data), " 1: [wt] project-feature")

    def test_main_checkout_title_is_unchanged(self):
        module = self.load("cattery_tab.py")
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory)
        root = directory / "project"
        (root / ".git").mkdir(parents=True)
        data = {"fmt": None, "title": str(root), "index": 1, "tab": FakeTab(str(root))}
        self.assertEqual(module.draw_title(data), f" 1: {root}")

    def test_missing_title_and_index_do_not_raise(self):
        module = self.load("cattery_tab.py")
        self.assertEqual(module.draw_title({"fmt": None}), " : ")


if __name__ == "__main__":
    unittest.main()
