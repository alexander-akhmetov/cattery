"""Unit tests for kitty/cattery_watcher.py, the kitty agent-state watcher.

The watcher imports kitty internals that exist only inside a running kitty, so
this stubs `kitty.boss`, `kitty.window`, `kitty.fast_data_types`, `kitty.utils`,
and `kitty.notifications` before loading it. Everything under test then runs
outside kitty. `_derive_display` is a function of state plus bookkeeping.
`_update_os_title` walks the tab manager and calls one C function, which the
stub records and refuses inside `FAIL_TITLE_WRITES`. `_apply` and the watcher
entry points drive `FakeWindow`, which stores user variables the way kitty does.

Notifications go through `boss.notification_manager`, which `FakeBoss` supplies
as a `RecordingNotifications`, so no test reaches a desktop. The transition
datagrams go through `socket.socket`, which `PublishTest` replaces with a
recorder, so no test opens one.

Run with `make test-python`.
"""

import enum
import errno
import importlib.util
import json
import os
import sys
import time
import types
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parent.parent

# What the stubbed get_options() reports as kitty.conf's `env` directives. The
# watcher reads CATTERY_BIN out of it to find the picker.
KITTY_ENV: dict[str, str] = {}

# Titles pushed through the stubbed set_os_window_title, as (os_window_id, title).
TITLE_CALLS: list[tuple[int, str]] = []

# Messages the watcher sent to kitty's log.
LOG_CALLS: list[str] = []

# The fields kitty's NotificationCommand carries, from
# `NotificationCommand.__annotations__` in kitty 0.48.1. The double below takes
# these and nothing else, so a write to a field kitty does not have fails here
# instead of going out as a notification missing its identifier or its buttons.
# A kitty upgrade that renames one is a change to this list.
NOTIFICATION_FIELDS = (
    "title",
    "body",
    "actions",
    "only_when",
    "urgency",
    "icon_data_key",
    "icon_names",
    "application_name",
    "notification_types",
    "timeout",
    "buttons",
    "sound_name",
    "on_activation",
    "on_close",
    "on_update",
    "identifier",
    "done",
    "channel_id",
    "desktop_notification_id",
    "close_response_requested",
    "icon_path",
    "current_payload_type",
    "current_payload_buffer",
    "created_by_desktop",
    "activation_token",
)

# Timers armed through the stubbed add_timer, as (callback, interval, repeats).
# The watcher imports add_timer inside a try, so without this stub the sweep
# would be silently disabled and every test about it would pass by doing
# nothing.
TIMERS: list[tuple] = []


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

    def add_timer(callback, interval, repeats):
        TIMERS.append((callback, interval, repeats))
        return len(TIMERS)

    fast_mod.set_os_window_title = set_os_window_title
    fast_mod.get_options = lambda: types.SimpleNamespace(env=KITTY_ENV)
    fast_mod.add_timer = add_timer

    utils_mod = types.ModuleType("kitty.utils")
    utils_mod.log_error = LOG_CALLS.append

    notifications_mod = types.ModuleType("kitty.notifications")
    notifications_mod.OnlyWhen = enum.Enum("OnlyWhen", ["unset", "always", "unfocused", "invisible"])
    notifications_mod.Urgency = enum.Enum("Urgency", ["Low", "Normal", "Critical"])

    sys.modules.update(
        {
            "kitty": kitty,
            "kitty.boss": boss_mod,
            "kitty.window": window_mod,
            "kitty.fast_data_types": fast_mod,
            "kitty.utils": utils_mod,
            "kitty.notifications": notifications_mod,
        }
    )

    path = REPO_ROOT / "kitty" / "cattery_watcher.py"
    spec = importlib.util.spec_from_file_location("cattery_watcher_under_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


watcher = _load_watcher()


class FakeChild:
    """kitty's process handle. The watcher reads one field off it."""

    def __init__(self, cwd):
        self.current_cwd = cwd


class FakeWindow:
    def __init__(
        self,
        window_id=1,
        display=None,
        title="",
        focused=False,
        state=None,
        kind=None,
        os_window_id=1,
        cwd="",
        msg=None,
        tool=None,
        tool_since=None,
    ):
        self.id = window_id
        self.os_window_id = os_window_id
        self.title = title
        self.is_focused = focused
        self.child = FakeChild(cwd)
        self.user_vars = {}
        if msg is not None:
            self.user_vars["AGENT_MSG"] = msg
        # Every write the watcher made, as (key, value), including the
        # deletions it makes by passing None.
        self.var_calls = []
        if display is not None:
            self.user_vars["AGENT_DISPLAY"] = display
        if state is not None:
            self.user_vars["AGENT_STATE"] = state
        if kind is not None:
            self.user_vars["AGENT_KIND"] = kind
        if tool is not None:
            self.user_vars["AGENT_TOOL"] = tool
        if tool_since is not None:
            self.user_vars["AGENT_TOOL_SINCE"] = tool_since

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


class FakeNotificationCommand:
    """What kitty hands out of create_notification_cmd, for filling in.

    __slots__ is the point: the real class has none, so `cmd.identifer = ...`
    would take the typo, raise nothing, and go out as a notification with no
    identifier. Here it raises.
    """

    __slots__ = NOTIFICATION_FIELDS


class RecordingNotifications:
    """Stand-in for boss.notification_manager.

    Setting `error` makes the send raise, which is the shape of kitty's
    internal API changing under the watcher.
    """

    def __init__(self):
        # Every send, as (cmd, channel_id).
        self.calls = []
        self.error = None

    def create_notification_cmd(self):
        return FakeNotificationCommand()

    def notify_with_command(self, cmd, channel_id):
        if self.error is not None:
            raise self.error
        self.calls.append((cmd, channel_id))
        return 1


class FakeBoss:
    def __init__(self, tab_manager=None, os_window_id=1, windows=()):
        self.os_window_map = {os_window_id: tab_manager} if tab_manager else {}
        self.notification_manager = RecordingNotifications()
        # The flat map the sweep and cattery_jump.py both walk.
        self.window_id_map = {w.id: w for w in windows}
        # Every focus, as (window, switch, token), and every launch, as its argv.
        self.focused = []
        self.launched = []

    def set_active_window(self, window, switch_os_window_if_needed=False, activation_token=""):
        self.focused.append((window, switch_os_window_if_needed, activation_token))

    def launch(self, *args):
        self.launched.append(args)


def boss_for(windows, active=None):
    """One OS window (id 1) holding one tab of `windows`."""
    return FakeBoss(FakeTabManager(windows, active=active), windows=windows)


class RecordingSocket:
    """Stand-in for the watcher's datagram socket.

    It records what was sent where, fails one path with an errno, or raises
    whatever `raises` holds for every send.
    """

    def __init__(self):
        self.sent = []  # (path, the decoded event)
        self.raw = []  # the bytes of each datagram, which has a size limit
        self.errors = {}  # path -> errno raised instead of sending
        self.raises = None
        self.blocking = None

    def setblocking(self, flag):
        self.blocking = flag

    def sendto(self, payload, path):
        if self.raises is not None:
            raise self.raises
        code = self.errors.get(path)
        if code is not None:
            raise OSError(code, os.strerror(code))
        self.sent.append((path, json.loads(payload)))
        self.raw.append(payload)
        return len(payload)

    def paths(self):
        return [path for path, _ in self.sent]

    def events(self):
        return [event for _, event in self.sent]


class SocketFactory:
    """Stand-in for socket.socket, handing out one recorder and counting calls."""

    def __init__(self, sock):
        self.sock = sock
        self.calls = 0

    def __call__(self, family, kind):
        self.calls += 1
        return self.sock


class WatcherTestCase(unittest.TestCase):
    """Base for the tests that reach titles and notifications."""

    def setUp(self):
        TITLE_CALLS.clear()
        KITTY_ENV.clear()
        LOG_CALLS.clear()


class DeriveDisplayTest(unittest.TestCase):
    def test_display_for_state(self):
        old = time.time() - watcher._STALL_THRESHOLD - 60
        fresh = time.time() - 5
        cases = [
            # name, state, prev, focused, seen_before, tool_since, want_display, want_seen_after
            ("working", "working", None, False, False, None, "working", False),
            ("working clears seen", "working", "done", False, True, None, "working", False),
            ("blocked", "blocked", "working", False, False, None, "blocked", False),
            ("blocked clears seen", "blocked", "idle", False, True, None, "blocked", False),
            ("finished unseen", "idle", "working", False, False, None, "done", False),
            ("finished after blocked", "idle", "blocked", False, False, None, "done", False),
            ("stays done until seen", "idle", "done", False, False, None, "done", False),
            ("finished while watched", "idle", "working", True, False, None, "idle", True),
            ("already acknowledged", "idle", "working", False, True, None, "idle", True),
            # An agent that announces idle at startup has finished nothing, so
            # it must not become "done".
            ("idle before any work", "idle", None, False, False, None, "idle", False),
            ("idle stays idle", "idle", "idle", False, False, None, "idle", False),
            ("no state", None, "working", False, False, None, None, False),
            ("unknown state", "thinking", "working", False, False, None, None, False),
            # A tool that has run past the threshold, and one that has not.
            ("a hung tool", "working", "working", False, False, old, "stalled", False),
            ("a tool inside the threshold", "working", "working", False, False, fresh, "working", False),
            ("stalled stays stalled", "working", "stalled", False, False, old, "stalled", False),
            # The tool ended, so the label goes and the agent is working again.
            ("a stalled tool that ended", "working", "stalled", False, False, None, "working", False),
            # An agent waiting on a question is not stuck, whatever the tool it
            # left open says.
            ("blocked ignores the tool", "blocked", "working", False, False, old, "blocked", False),
            # _WORKED has to carry "stalled", or the run finishes with no marker
            # and no notification. Silently: that is the agent you most wanted
            # to hear about.
            ("a stalled agent finishes", "idle", "stalled", False, False, None, "done", False),
        ]
        for name, state, prev, focused, seen_before, tool_since, want, want_seen in cases:
            with self.subTest(name):
                seen = {7} if seen_before else set()
                got = watcher._derive_display(state, 7, seen, focused, prev, tool_since)
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
        self.assertEqual(len(boss.notification_manager.calls), 1, "one notification per edge")

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
                window = FakeWindow(1, display=prev, state=state, kind="claude", focused=focused)
                boss = boss_for([window])

                watcher._apply(boss, window)

                self.assertEqual(window.user_vars.get("AGENT_DISPLAY"), want_display)
                self.assertEqual(len(boss.notification_manager.calls), 1 if want_notification else 0)

    def test_the_notification_names_the_agent_and_the_window(self):
        KITTY_ENV["CATTERY_BIN"] = "/opt/homebrew/bin/cattery"
        window = FakeWindow(1, display="working", state="idle", kind="pi", title="~/projects/cattery")
        boss = boss_for([window])

        watcher._apply(boss, window)

        cmd, channel_id = boss.notification_manager.calls[0]
        self.assertEqual(cmd.title, "Agent finished (pi)")
        self.assertEqual(cmd.body, "~/projects/cattery")
        self.assertEqual(channel_id, window.id)
        # One identifier per window and state, so a repeat replaces its own
        # banner instead of stacking, and blocked and done coexist.
        self.assertEqual(cmd.identifier, "cattery-1-done")
        self.assertEqual(cmd.sound_name, "silent")
        self.assertEqual(cmd.buttons, ("Open picker",))
        # Asking kitty for no action of its own is what keeps a button press
        # from also focusing the agent window.
        self.assertEqual(cmd.actions, frozenset())

    def test_the_button_is_left_off_when_there_is_nothing_to_launch(self):
        # An install predating the `env CATTERY_BIN` line in kitty.conf. A
        # button that silently does nothing is worse than no button.
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])

        watcher._apply(boss, window)

        cmd, _ = boss.notification_manager.calls[0]
        self.assertEqual(cmd.buttons, ())

    def test_a_failing_notification_still_leaves_the_state_published(self):
        # kitty's notification API is internal, and an older kitty has none at
        # all. The tab marker is the part that has to survive.
        cases = [
            # name, break it, whether the failure is logged
            ("no manager", lambda boss: delattr(boss, "notification_manager"), False),
            ("send raises", lambda boss: setattr(boss.notification_manager, "error", RuntimeError("gone")), True),
            # What the guarded import leaves behind on a kitty too old to have
            # kitty.notifications at all.
            ("no notification module", lambda boss: setattr(watcher, "OnlyWhen", None), False),
        ]
        self.addCleanup(setattr, watcher, "OnlyWhen", watcher.OnlyWhen)
        for name, break_it, want_log in cases:
            with self.subTest(name):
                LOG_CALLS.clear()
                window = FakeWindow(1, display="working", state="idle", kind="claude")
                boss = boss_for([window])
                # Held onto, because a case is free to take it off the boss.
                manager = boss.notification_manager
                break_it(boss)

                watcher._apply(boss, window)

                self.assertEqual(window.user_vars["AGENT_DISPLAY"], "done")
                self.assertEqual(manager.calls, [], "a half-built notification went out")
                # An API that changed shape leaves a trace in kitty's log.
                # Having none at all is expected, not a failure to report.
                self.assertEqual(len(LOG_CALLS), 1 if want_log else 0)


class ActivationTest(WatcherTestCase):
    """What a press on the banner does. Button 0 is the body, 1 the button."""

    def _activation(self, window, boss):
        watcher._apply(boss, window)
        cmd, _ = boss.notification_manager.calls[0]
        return cmd.on_activation

    def _pressed(self, token=""):
        """The command kitty hands back, carrying whatever token it collected."""
        cmd = FakeNotificationCommand()
        cmd.activation_token = token
        return cmd

    def test_the_body_focuses_the_window_it_came_from(self):
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])

        self._activation(window, boss)(self._pressed(), 0)

        # switch_os_window_if_needed, or an agent in another OS window stays
        # hidden behind the one in front.
        self.assertEqual(boss.focused, [(window, True, "")])
        self.assertEqual(boss.launched, [])

    def test_the_focus_carries_the_activation_token(self):
        # kitty's own focus path is off, and it is the only other thing that
        # passes the token on. Without one, a Wayland compositor with
        # focus-stealing prevention drops the raise and the marker stays.
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])

        self._activation(window, boss)(self._pressed("xdg-token"), 0)

        self.assertEqual(boss.focused, [(window, True, "xdg-token")])

    def test_a_window_that_closed_first_is_ignored(self):
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])
        activation = self._activation(window, boss)
        del boss.window_id_map[window.id]

        activation(self._pressed(), 0)

        self.assertEqual(boss.focused, [])

    def test_the_button_opens_the_picker_without_focusing_the_agent(self):
        KITTY_ENV["CATTERY_BIN"] = "/opt/homebrew/bin/cattery"
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])

        self._activation(window, boss)(self._pressed(), 1)

        self.assertEqual(boss.launched, [("--type=overlay", "--copy-colors", "/opt/homebrew/bin/cattery")])
        self.assertEqual(boss.focused, [], "the button must not drag the user to the agent")

    def test_an_install_with_no_binary_path_launches_nothing(self):
        # The banner carries no button in this case, but kitty.conf can also
        # lose the line between the notification and the press.
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])

        self._activation(window, boss)(self._pressed(), 1)

        self.assertEqual(boss.launched, [])


class PublishTest(WatcherTestCase):
    """The transition datagrams, and what a bad subscriber can cost kitty."""

    def setUp(self):
        super().setUp()
        self.sock = RecordingSocket()
        self.factory = SocketFactory(self.sock)
        # The watcher keeps one socket for the life of the process, so a test
        # that made it must not leave it for the next one.
        watcher._sender = None
        self.addCleanup(setattr, watcher, "_sender", None)
        patcher = mock.patch.object(watcher.socket, "socket", self.factory)
        patcher.start()
        self.addCleanup(patcher.stop)

    def subscribe(self, boss, *paths):
        watcher._ensure_state(boss)
        for path in paths:
            boss._agent_subs[path] = None

    def test_a_transition_carries_the_agent_and_its_prompt(self):
        window = FakeWindow(
            363,
            display="working",
            state="blocked",
            kind="pi",
            title="~/projects/sigil",
            cwd="/Users/x/projects/sigil",
            msg="fix the picker",
        )
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)

        self.assertEqual(self.sock.paths(), ["/tmp/one.sock"])
        event = self.sock.events()[0]
        self.assertEqual(event["from"], "working")
        self.assertEqual(event["to"], "blocked")
        self.assertEqual(event["window"], 363)
        self.assertEqual(event["kind"], "pi")
        self.assertEqual(event["title"], "~/projects/sigil")
        self.assertEqual(event["cwd"], "/Users/x/projects/sigil")
        self.assertEqual(event["msg"], "fix the picker")
        self.assertIs(event["focused"], False)
        self.assertAlmostEqual(event["ts"], int(time.time()), delta=5)

    def test_the_event_is_one_compact_utf8_object(self):
        window = FakeWindow(1, display="working", state="blocked", kind="pi", msg="почини пикер")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)

        payload = self.sock.raw[0]
        self.assertTrue(payload.startswith(b'{"ts":'), payload[:16])
        self.assertIn(b'"kind":"pi"', payload, "no padding between the fields")
        self.assertIn("почини пикер".encode(), payload, "the text as UTF-8, not as \\u escapes")

    def test_an_event_fits_in_one_datagram(self):
        # macOS refuses a unix datagram over net.local.dgram.maxdgram, 2048
        # bytes by default, and its EMSGSIZE reads here as a subscriber that is
        # behind: the event would go missing with nothing said. A title is
        # whatever the program in the window set.
        window = FakeWindow(
            1,
            display="working",
            state="blocked",
            kind="pi",
            title="ф" * 900,
            cwd="/Users/x/projects/sigil",
            msg="п" * 900,
        )
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)

        self.assertLess(len(self.sock.raw[0]), 2048)
        event = self.sock.events()[0]
        self.assertEqual(len(event["title"]), 200)
        self.assertTrue(event["title"].endswith("…"))
        self.assertEqual(len(event["msg"]), 200)

    def test_the_first_display_of_a_window_has_no_previous_one(self):
        window = FakeWindow(1, state="working", kind="claude")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)

        self.assertIsNone(self.sock.events()[0]["from"])

    def test_each_subscriber_gets_its_own_copy(self):
        window = FakeWindow(1, display="working", state="blocked", kind="claude")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock", "/tmp/two.sock")

        watcher._apply(boss, window)

        self.assertEqual(self.sock.paths(), ["/tmp/one.sock", "/tmp/two.sock"])
        self.assertEqual(self.sock.events()[0], self.sock.events()[1])

    def test_only_a_changed_display_is_an_event(self):
        window = FakeWindow(1, state="blocked", kind="claude")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)
        watcher._apply(boss, window)

        self.assertEqual(len(self.sock.sent), 1)

    def test_a_cleared_state_carries_no_prompt(self):
        window = FakeWindow(1, display="working", state="working", kind="claude", msg="fix the picker")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")
        # Both writers delete AGENT_MSG before AGENT_STATE, and deleting
        # AGENT_STATE is what runs the watcher, so the prompt is already gone.
        del window.user_vars["AGENT_MSG"]
        del window.user_vars["AGENT_STATE"]

        watcher._apply(boss, window)

        event = self.sock.events()[0]
        self.assertEqual(event["from"], "working")
        self.assertEqual(event["to"], "cleared")
        self.assertEqual(event["msg"], "")

    def test_a_window_that_never_opted_in_reports_nothing(self):
        window = FakeWindow(1)
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)
        watcher.on_close(boss, window, {})

        self.assertEqual(self.sock.sent, [])

    def test_a_closing_window_is_reported(self):
        window = FakeWindow(1, display="blocked", title="agent")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher.on_close(boss, window, {})

        event = self.sock.events()[0]
        self.assertEqual(event["from"], "blocked")
        self.assertEqual(event["to"], "closed")

    def test_a_departed_subscriber_is_pruned(self):
        for name, code in (("no socket at the path", errno.ENOENT), ("nobody bound to it", errno.ECONNREFUSED)):
            with self.subTest(name):
                self.sock.sent.clear()
                self.sock.errors = {"/tmp/gone.sock": code}
                window = FakeWindow(1, display="working", state="blocked", kind="claude")
                boss = boss_for([window])
                self.subscribe(boss, "/tmp/gone.sock", "/tmp/alive.sock")

                watcher._apply(boss, window)

                self.assertEqual(list(boss._agent_subs), ["/tmp/alive.sock"])
                self.assertEqual(self.sock.paths(), ["/tmp/alive.sock"])
                self.assertEqual(window.user_vars["AGENT_DISPLAY"], "blocked", "the marker still moved")

    def test_a_subscriber_that_is_behind_keeps_its_registration(self):
        self.sock.errors = {"/tmp/slow.sock": errno.ENOBUFS}
        window = FakeWindow(1, display="working", state="blocked", kind="claude")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/slow.sock")

        watcher._apply(boss, window)

        self.assertEqual(list(boss._agent_subs), ["/tmp/slow.sock"], "alive, just behind")
        self.assertEqual(self.sock.sent, [], "that datagram is dropped")

    def test_a_raising_send_costs_nothing(self):
        self.sock.raises = RuntimeError("the socket layer misbehaved")
        window = FakeWindow(1, display="working", state="idle", kind="claude")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "done")
        self.assertEqual(boss.os_window_map[1].dirty, 1)
        self.assertEqual(len(boss.notification_manager.calls), 1, "the notification still fired")

    def test_nobody_registered_means_no_socket_at_all(self):
        window = FakeWindow(1, state="working", kind="claude")
        boss = boss_for([window])

        watcher._apply(boss, window)

        self.assertEqual(self.factory.calls, 0)
        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "working", "the rest is unchanged")

    def test_one_non_blocking_socket_serves_every_event(self):
        # A blocking send on kitty's own thread would freeze the terminal.
        window = FakeWindow(1, state="working", kind="claude")
        boss = boss_for([window])
        self.subscribe(boss, "/tmp/one.sock")

        watcher._apply(boss, window)
        window.user_vars["AGENT_STATE"] = "blocked"
        watcher._apply(boss, window)

        self.assertEqual(self.factory.calls, 1)
        self.assertIs(self.sock.blocking, False)
        self.assertEqual(len(self.sock.sent), 2)


class SweepTest(WatcherTestCase):
    """The timer behind "stalled", which no event can reach."""

    def setUp(self):
        super().setUp()
        TIMERS.clear()

    def hung(self, **kwargs):
        """A working window whose one tool call has outlived the threshold."""
        since = str(int(time.time() - watcher._STALL_THRESHOLD - 60))
        return FakeWindow(state="working", kind="pi", tool="bash: sleep 900", tool_since=since, **kwargs)

    def test_a_hung_tool_reaches_stalled_with_no_event(self):
        window = self.hung(display="working")
        window.user_vars["AGENT_SINCE"] = "1700000000"
        boss = boss_for([window])

        watcher._sweep(boss)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "stalled")
        # working and stalled are one turn seen twice. Restamping would zero the
        # tab's elapsed minutes exactly when the number starts mattering.
        self.assertEqual(window.user_vars["AGENT_SINCE"], "1700000000")
        self.assertEqual(len(boss.notification_manager.calls), 1, "an unfocused stall is worth a notification")

    def test_a_dead_pis_tool_does_not_stall_the_next_agent(self):
        # `cattery state clear` drops AGENT_STATE, AGENT_KIND and AGENT_MSG and
        # nothing else, so a pi killed mid-call leaves its label on the window.
        # Without the kind test the Claude started there would go stalled, and
        # notify, inside its first second.
        window = self.hung(display="working")
        window.user_vars["AGENT_KIND"] = "claude"
        boss = boss_for([window])

        watcher._sweep(boss)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "working")
        self.assertEqual(boss.notification_manager.calls, [], "and no notification either")

    def test_the_sweep_leaves_other_windows_alone(self):
        # _apply's "no display" branch marks the tab bar dirty and rewrites the
        # OS-window title whatever it finds, so an unfiltered sweep would do
        # both for every window in the process once a minute, forever.
        shell = FakeWindow(1, title="fish")
        idle = FakeWindow(2, display="idle", state="idle", kind="pi")
        boss = boss_for([shell, idle])

        watcher._sweep(boss)

        self.assertEqual(shell.var_calls, [])
        self.assertEqual(idle.var_calls, [])
        self.assertEqual(boss.os_window_map[1].dirty, 0)
        self.assertEqual(TITLE_CALLS, [])

    def test_a_fresh_tool_is_left_working(self):
        window = FakeWindow(
            display="working", state="working", kind="pi", tool="bash: go test", tool_since=str(int(time.time()))
        )
        boss = boss_for([window])

        watcher._sweep(boss)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "working")
        self.assertEqual(window.var_calls, [])

    def test_the_threshold_is_ten_minutes(self):
        # The same ages as TestStalled in internal/agent/agent_test.go, written
        # out rather than derived from _STALL_THRESHOLD: the picker and the tab
        # bar hold one rule in two languages, and only these two tables stop the
        # numbers drifting apart.
        for age, want in ((9 * 60, "working"), (11 * 60, "stalled")):
            with self.subTest(age=age):
                window = FakeWindow(
                    display="working",
                    state="working",
                    kind="pi",
                    tool="bash: sleep 900",
                    tool_since=str(int(time.time() - age)),
                )

                watcher._sweep(boss_for([window]))

                self.assertEqual(window.user_vars["AGENT_DISPLAY"], want)

    def test_on_load_arms_one_repeating_timer(self):
        boss = boss_for([])

        watcher.on_load(boss, {})
        # A config reload re-executes the module against the same boss, and a
        # second timer would sweep twice a minute forever.
        watcher.on_load(boss, {})

        self.assertEqual(len(TIMERS), 1)
        _callback, interval, repeats = TIMERS[0]
        self.assertEqual(interval, watcher._SWEEP_INTERVAL)
        self.assertTrue(repeats)

    def test_the_timer_sweeps(self):
        window = self.hung(display="working")
        boss = boss_for([window])
        watcher.on_load(boss, {})

        # kitty passes the timer id; the callback takes whatever it is given.
        TIMERS[0][0](7)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "stalled")

    def test_a_raising_window_does_not_take_the_timer_with_it(self):
        class Exploding:
            id = 1
            user_vars = property(lambda self: (_ for _ in ()).throw(RuntimeError("gone")))

        watcher._sweep(FakeBoss(windows=[Exploding()]))

    def test_a_kitty_without_add_timer_still_loads(self):
        # add_timer is exported by kitty 0.48.1 but documented nowhere, so an
        # older or a future kitty may not carry it. The picker derives stalled
        # for itself either way; only the tab marker is lost.
        fast = sys.modules["kitty.fast_data_types"]
        self.addCleanup(setattr, fast, "add_timer", fast.add_timer)
        del fast.add_timer
        boss = boss_for([])

        watcher.on_load(boss, {})

        self.assertIsNone(boss._agent_timer)
        self.assertEqual(TIMERS, [])

    def test_a_stalled_agent_that_finishes_still_reports_done(self):
        window = self.hung(display="stalled")
        boss = boss_for([window])
        window.user_vars["AGENT_STATE"] = "idle"

        watcher._apply(boss, window)

        self.assertEqual(window.user_vars["AGENT_DISPLAY"], "done")
        self.assertEqual(len(boss.notification_manager.calls), 1, "the marker and the notification are the point")


class EntryPointTest(WatcherTestCase):
    """The callbacks kitty invokes."""

    def test_only_the_agent_input_keys_are_applied(self):
        cases = [
            ("AGENT_STATE", True),
            ("AGENT_KIND", True),
            # A tool boundary can move the agent in and out of "stalled", so it
            # has to be recomputed rather than left to the next sweep.
            ("AGENT_TOOL", True),
            # Its timestamp is written first, so reacting to it would apply the
            # new stamp against the previous label.
            ("AGENT_TOOL_SINCE", False),
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
