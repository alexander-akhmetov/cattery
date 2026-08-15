"""Unit tests for kitty/cattery_watcher.py, the kitty agent-state watcher.

The watcher imports kitty internals that exist only inside a running kitty, so
this stubs `kitty.boss`, `kitty.window`, and `kitty.fast_data_types` before
loading it. Everything under test then runs outside kitty. `_derive_display` is
a function of state plus bookkeeping. `_update_os_title` walks the tab manager
and calls one C function, which the stub records and refuses inside
`FAIL_TITLE_WRITES`. `_apply` and the watcher entry points drive `FakeWindow`,
which stores user variables the way kitty does.

Notifications go through `subprocess.Popen`, which `WatcherTestCase` replaces
with a recorder, so no test starts terminal-notifier.

Run with `make test-python`.
"""

import importlib.util
import sys
import time
import types
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parent.parent

# Titles pushed through the stubbed set_os_window_title, as (os_window_id, title).
TITLE_CALLS: list[tuple[int, str]] = []


class _FailTitleWrites:
    """Context manager making the stubbed set_os_window_title raise."""

    active = False

    def __enter__(self):
        type(self).active = True
        return self

    def __exit__(self, *exc):
        type(self).active = False
        return False


FAIL_TITLE_WRITES = _FailTitleWrites()


def _load_watcher():
    kitty = types.ModuleType("kitty")
    kitty.__path__ = []  # mark as a package so submodule imports resolve
    boss_mod = types.ModuleType("kitty.boss")
    boss_mod.Boss = object
    window_mod = types.ModuleType("kitty.window")
    window_mod.Window = object
    fast_mod = types.ModuleType("kitty.fast_data_types")

    def set_os_window_title(os_window_id, title):
        if _FailTitleWrites.active:
            raise OSError("title write refused")
        TITLE_CALLS.append((os_window_id, title))

    fast_mod.set_os_window_title = set_os_window_title
    sys.modules.update(
        {
            "kitty": kitty,
            "kitty.boss": boss_mod,
            "kitty.window": window_mod,
            "kitty.fast_data_types": fast_mod,
        }
    )

    path = REPO_ROOT / "kitty" / "cattery_watcher.py"
    spec = importlib.util.spec_from_file_location("cattery_watcher_under_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


watcher = _load_watcher()


class FakeWindow:
    def __init__(self, window_id=1, display=None, title="", focused=False, state=None, kind=None, os_window_id=1):
        self.id = window_id
        self.os_window_id = os_window_id
        self.title = title
        self.is_focused = focused
        self.user_vars = {}
        # Every write the watcher made, as (key, value), including the
        # deletions it makes by passing None.
        self.var_calls = []
        if display is not None:
            self.user_vars["AGENT_DISPLAY"] = display
        if state is not None:
            self.user_vars["AGENT_STATE"] = state
        if kind is not None:
            self.user_vars["AGENT_KIND"] = kind

    def set_user_var(self, key, val=None):
        # kitty drops the key first and stores a value only when there is one,
        # so set_user_var(key, None) deletes the variable.
        self.var_calls.append((key, val))
        self.user_vars.pop(key, None)
        if val is not None:
            self.user_vars[key] = val

    def keys_written(self):
        return [key for key, _ in self.var_calls]


class FakeTab:
    def __init__(self, windows):
        self._windows = windows

    def __iter__(self):
        return iter(self._windows)


class FakeTabManager:
    def __init__(self, windows, active=None):
        self._tabs = [FakeTab(windows)]
        self.active_window = active if active is not None else (windows[0] if windows else None)
        self.dirty = 0

    def __iter__(self):
        return iter(self._tabs)

    def mark_tab_bar_dirty(self):
        self.dirty += 1


class FakeBoss:
    def __init__(self, tab_manager=None, os_window_id=1):
        self.os_window_map = {os_window_id: tab_manager} if tab_manager else {}


def boss_for(windows, active=None):
    """One OS window (id 1) holding one tab of `windows`."""
    return FakeBoss(FakeTabManager(windows, active=active))


class RecordingPopen:
    """Stand-in for subprocess.Popen. It records argv, or raises `error`."""

    def __init__(self):
        self.calls = []
        self.error = None

    def __call__(self, argv, **kwargs):
        if self.error is not None:
            raise self.error
        self.calls.append(argv)
        return None


class WatcherTestCase(unittest.TestCase):
    """Base for the tests that reach titles and notifications."""

    def setUp(self):
        TITLE_CALLS.clear()
        self.popen = RecordingPopen()
        patcher = mock.patch.object(watcher.subprocess, "Popen", self.popen)
        patcher.start()
        self.addCleanup(patcher.stop)


class DeriveDisplayTest(unittest.TestCase):
    def test_display_for_state(self):
        cases = [
            # name, state, prev, focused, seen_before, want_display, want_seen_after
            ("working", "working", None, False, False, "working", False),
            ("working clears seen", "working", "done", False, True, "working", False),
            ("blocked", "blocked", "working", False, False, "blocked", False),
            ("blocked clears seen", "blocked", "idle", False, True, "blocked", False),
            ("finished unseen", "idle", "working", False, False, "done", False),
            ("finished after blocked", "idle", "blocked", False, False, "done", False),
            ("stays done until seen", "idle", "done", False, False, "done", False),
            ("finished while watched", "idle", "working", True, False, "idle", True),
            ("already acknowledged", "idle", "working", False, True, "idle", True),
            # An agent that announces idle at startup has finished nothing, so
            # it must not become "done".
            ("idle before any work", "idle", None, False, False, "idle", False),
            ("idle stays idle", "idle", "idle", False, False, "idle", False),
            ("no state", None, "working", False, False, None, False),
            ("unknown state", "thinking", "working", False, False, None, False),
        ]
        for name, state, prev, focused, seen_before, want, want_seen in cases:
            with self.subTest(name):
                seen = {7} if seen_before else set()
                got = watcher._derive_display(state, 7, seen, focused, prev)
                self.assertEqual(got, want)
                self.assertEqual(7 in seen, want_seen, "seen bookkeeping")


class UpdateOsTitleTest(unittest.TestCase):
    def setUp(self):
        TITLE_CALLS.clear()

    def test_counts_windows_needing_attention(self):
        cases = [
            ("none", [None, "working"], []),
            ("one blocked", ["blocked", "working"], [(1, "(1 need you) shell")]),
            ("done counts too", ["done", "idle"], [(1, "(1 need you) shell")]),
            ("several", ["blocked", "done", "working"], [(1, "(2 need you) shell")]),
        ]
        for name, displays, want in cases:
            with self.subTest(name):
                TITLE_CALLS.clear()
                windows = [FakeWindow(i, d) for i, d in enumerate(displays)]
                active = FakeWindow(99, None, title="shell")
                boss = FakeBoss(FakeTabManager(windows, active=active))
                watcher._update_os_title(boss, 1)
                self.assertEqual(TITLE_CALLS, want)

    def test_leaves_untouched_titles_alone(self):
        boss = FakeBoss(FakeTabManager([FakeWindow(1, "working")]))
        watcher._update_os_title(boss, 1)
        self.assertEqual(TITLE_CALLS, [], "must not reset a title it never set")

    def test_restores_default_once_attention_clears(self):
        window = FakeWindow(1, "blocked", title="agent")
        boss = FakeBoss(FakeTabManager([window]))

        watcher._update_os_title(boss, 1)
        window.user_vars["AGENT_DISPLAY"] = "idle"
        watcher._update_os_title(boss, 1)
        watcher._update_os_title(boss, 1)

        self.assertEqual(TITLE_CALLS, [(1, "(1 need you) agent"), (1, "")])

    def test_follows_the_active_window_title(self):
        active = FakeWindow(2, None, title="~/projects/dotfiles")
        boss = FakeBoss(FakeTabManager([FakeWindow(1, "blocked")], active=active))

        watcher._update_os_title(boss, 1)
        watcher._update_os_title(boss, 1)  # unchanged: no repeat write
        active.title = "~/projects/sigil"
        watcher._update_os_title(boss, 1)

        self.assertEqual(
            TITLE_CALLS,
            [(1, "(1 need you) ~/projects/dotfiles"), (1, "(1 need you) ~/projects/sigil")],
        )

    def test_ignores_the_window_being_closed(self):
        # kitty calls the close watcher while the window is still in its tab.
        # The count has to leave it out, or the prefix outlives the agent.
        closing = FakeWindow(1, "blocked", title="agent")
        other = FakeWindow(2, "working")
        boss = FakeBoss(FakeTabManager([closing, other], active=other))

        watcher._update_os_title(boss, 1, closing_window_id=closing.id)

        self.assertEqual(TITLE_CALLS, [])

    def test_retries_after_a_failed_write(self):
        window = FakeWindow(1, "blocked", title="agent")
        boss = FakeBoss(FakeTabManager([window]))

        with FAIL_TITLE_WRITES:
            watcher._update_os_title(boss, 1)
        self.assertEqual(TITLE_CALLS, [], "a failed write records nothing")

        watcher._update_os_title(boss, 1)
        self.assertEqual(TITLE_CALLS, [(1, "(1 need you) agent")])

    def test_closed_os_window_drops_its_bookkeeping(self):
        boss = FakeBoss()
        watcher._ensure_state(boss)
        boss._agent_titles[42] = "(1 need you) agent"

        watcher._update_os_title(boss, 42)

        self.assertEqual(TITLE_CALLS, [])
        self.assertNotIn(42, boss._agent_titles)


class ApplyTest(WatcherTestCase):
    """_apply is where the derived display is published and fanned out."""

    def test_publishes_display_and_timestamp(self):
        window = FakeWindow(1, state="working", kind="pi")
        boss = boss_for([window])

        watcher._apply(boss, window)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "working")
        self.assertAlmostEqual(int(window.user_vars["AGENT_SINCE"]), int(time.time()), delta=5)
        self.assertEqual(boss.os_window_map[1].dirty, 1, "the tab bar has to be redrawn")

    def test_the_same_state_written_again_changes_nothing(self):
        # Claude's hooks resend AGENT_KIND on every call, so _apply runs again
        # with the display it already published. A second write would move
        # AGENT_SINCE and restart the tab bar's elapsed counter.
        window = FakeWindow(1, state="blocked", kind="claude")
        boss = boss_for([window])

        watcher._apply(boss, window)
        watcher._apply(boss, window)

        self.assertEqual(window.keys_written(), ["AGENT_DISPLAY", "AGENT_SINCE"])
        self.assertEqual(len(self.popen.calls), 1, "one notification per edge")

    def test_a_cleared_state_drops_the_watcher_variables(self):
        window = FakeWindow(1, state="working", kind="claude")
        boss = boss_for([window])
        watcher._apply(boss, window)

        del window.user_vars["AGENT_STATE"]
        watcher._apply(boss, window)

        self.assertNotIn("AGENT_DISPLAY", window.user_vars)
        self.assertNotIn("AGENT_SINCE", window.user_vars)
        self.assertEqual(boss.os_window_map[1].dirty, 2)

    def test_a_window_that_never_opted_in_is_left_alone(self):
        window = FakeWindow(1)
        boss = boss_for([window])

        watcher._apply(boss, window)

        self.assertEqual(window.var_calls, [], "no AGENT_STATE, nothing to clear")

    def test_notifies_only_on_an_unseen_edge_into_attention(self):
        cases = [
            # name, state, prev display, focused, want display, want notification
            ("work starts quietly", "working", None, False, "working", False),
            ("blocked in an unwatched window", "blocked", "working", False, "blocked", True),
            ("blocked in the window you are looking at", "blocked", "working", True, "blocked", False),
            ("finished unseen", "idle", "working", False, "done", True),
            ("finished while watched", "idle", "working", True, "idle", False),
            ("still blocked, no edge", "blocked", "blocked", False, "blocked", False),
        ]
        for name, state, prev, focused, want_display, want_notification in cases:
            with self.subTest(name):
                self.popen.calls.clear()
                window = FakeWindow(1, display=prev, state=state, kind="claude", focused=focused)
                boss = boss_for([window])

                watcher._apply(boss, window)

                self.assertEqual(window.user_vars.get("AGENT_DISPLAY"), want_display)
                self.assertEqual(len(self.popen.calls), 1 if want_notification else 0)

    def test_the_notification_names_the_agent_and_the_window(self):
        window = FakeWindow(1, display="working", state="idle", kind="pi", title="~/projects/cattery")
        boss = boss_for([window])

        watcher._apply(boss, window)

        argv = self.popen.calls[0]
        self.assertEqual(argv[0], "terminal-notifier")
        self.assertIn("Agent finished (pi)", argv)
        self.assertIn("~/projects/cattery", argv)
        # One group per window and state, so a repeat replaces instead of
        # stacking.
        self.assertIn("kitty-agent-1-done", argv)

    def test_a_missing_notifier_still_leaves_the_state_published(self):
        # Linux has no terminal-notifier, and a sandbox can block it. The tab
        # marker is the part that has to survive.
        self.popen.error = FileNotFoundError("terminal-notifier")
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])

        watcher._apply(boss, window)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "done")


class EntryPointTest(WatcherTestCase):
    """The callbacks kitty invokes."""

    def test_only_the_agent_input_keys_are_applied(self):
        cases = [
            ("AGENT_STATE", True),
            ("AGENT_KIND", True),
            # The watcher's own writes come back through this callback, and
            # applying them would recurse.
            ("AGENT_DISPLAY", False),
            ("AGENT_SINCE", False),
            ("SOMETHING_ELSE", False),
            (None, False),
        ]
        for key, want_applied in cases:
            with self.subTest(key=key):
                window = FakeWindow(1, state="working", kind="claude")
                boss = boss_for([window])

                watcher.on_set_user_var(boss, window, {"key": key})

                self.assertEqual(bool(window.var_calls), want_applied)

    def test_focus_marks_the_window_seen_and_drops_done(self):
        # The event says the window is focused. window.is_focused is the
        # watcher's other source for the same fact, and the callback can run
        # before kitty flips it. The marker has to clear either way, which is
        # what the bookkeeping in on_focus_change buys: _apply alone would read
        # is_focused, see False, and keep the window unseen.
        for name, is_focused in (("attribute already flipped", True), ("attribute not flipped yet", False)):
            with self.subTest(name):
                window = FakeWindow(1, display="done", state="idle", kind="claude", focused=is_focused)
                boss = boss_for([window])

                watcher.on_focus_change(boss, window, {"focused": True})

                self.assertEqual(window.user_vars["AGENT_DISPLAY"], "idle")
                self.assertIn(1, boss._agent_seen)

    def test_the_picker_can_mark_a_window_seen(self):
        # The set of seen windows lives in this process, so the read-write
        # preview cannot reach it directly. It publishes AGENT_SEEN and the
        # watcher picks it up, which is the only way in from outside kitty.
        window = FakeWindow(1, display="done", state="idle", kind="claude")
        boss = boss_for([window])
        window.set_user_var("AGENT_SEEN", "1")

        watcher.on_set_user_var(boss, window, {"key": "AGENT_SEEN"})

        self.assertIn(1, boss._agent_seen)
        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "idle", "the marker did not drop")
        # Left behind, the variable would travel into a session snapshot and
        # the next mark would have nothing to change.
        self.assertNotIn("AGENT_SEEN", window.user_vars)

    def test_clearing_the_seen_marker_does_not_recurse(self):
        # The watcher's own clearing write comes back through this callback.
        window = FakeWindow(1, display="done", state="idle", kind="claude")
        boss = boss_for([window])

        watcher.on_set_user_var(boss, window, {"key": "AGENT_SEEN"})

        self.assertEqual(boss._agent_seen, set())
        self.assertEqual(window.var_calls, [])

    def test_losing_focus_changes_nothing(self):
        window = FakeWindow(1, display="done", state="idle", kind="claude")
        boss = boss_for([window])

        watcher.on_focus_change(boss, window, {"focused": False})

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "done")
        self.assertEqual(boss._agent_seen, set())

    def test_focusing_a_shell_only_refreshes_the_title(self):
        # The prefix quotes the active window, so a focus change in a window
        # with no agent still leaves it naming the wrong one.
        shell = FakeWindow(2, title="fish", focused=True)
        blocked = FakeWindow(1, "blocked")
        boss = boss_for([blocked, shell], active=shell)

        watcher.on_focus_change(boss, shell, {"focused": True})

        self.assertEqual(shell.var_calls, [])
        self.assertEqual(boss._agent_seen, set(), "a shell is not an agent to acknowledge")
        self.assertEqual(TITLE_CALLS, [(1, "(1 need you) fish")])

    def test_a_retitled_window_refreshes_the_prefix(self):
        blocked = FakeWindow(1, "blocked")
        active = FakeWindow(2, title="fish")
        boss = boss_for([blocked, active], active=active)

        watcher.on_title_change(boss, active, {})
        active.title = "~/projects/cattery"
        watcher.on_title_change(boss, active, {})

        self.assertEqual(TITLE_CALLS, [(1, "(1 need you) fish"), (1, "(1 need you) ~/projects/cattery")])

    def test_closing_the_last_agent_forgets_it_and_clears_the_prefix(self):
        closing = FakeWindow(1, "blocked", title="agent")
        boss = boss_for([closing])
        watcher._update_os_title(boss, 1)
        watcher._ensure_state(boss)
        boss._agent_seen.add(1)

        watcher.on_close(boss, closing, {})

        self.assertNotIn(1, boss._agent_seen)
        self.assertEqual(TITLE_CALLS, [(1, "(1 need you) agent"), (1, "")])
        self.assertEqual(boss.os_window_map[1].dirty, 1)


if __name__ == "__main__":
    unittest.main()
