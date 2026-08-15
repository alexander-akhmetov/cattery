"""Unit tests for kitty/cattery_events.py, the subscriber registration kitten.

The kitten imports kitty internals that exist only inside a running kitty, so
this stubs `kitty.boss` and `kittens.tui.handler` before loading it. The stubbed
`result_handler` is a no-op decorator, which leaves `handle_result` callable
with the four arguments kitty passes it.

Run with `make test-python`.
"""

import importlib.util
import sys
import types
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent


def _load_kitten():
    kitty = sys.modules.get("kitty") or types.ModuleType("kitty")
    kitty.__path__ = []
    boss_mod = types.ModuleType("kitty.boss")
    boss_mod.Boss = object

    kittens = types.ModuleType("kittens")
    kittens.__path__ = []
    tui = types.ModuleType("kittens.tui")
    tui.__path__ = []
    handler = types.ModuleType("kittens.tui.handler")
    handler.result_handler = lambda *args, **kwargs: (lambda fn: fn)

    sys.modules.update(
        {
            "kitty": kitty,
            "kitty.boss": boss_mod,
            "kittens": kittens,
            "kittens.tui": tui,
            "kittens.tui.handler": handler,
        }
    )

    path = REPO_ROOT / "kitty" / "cattery_events.py"
    spec = importlib.util.spec_from_file_location("cattery_events_under_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


kitten = _load_kitten()


def run(boss, *args):
    """Call the kitten the way kitty does, with its own path in args[0]."""
    return kitten.handle_result(["/k/cattery_events.py", *args], "", 1, boss)


class RegistryTest(unittest.TestCase):
    def setUp(self):
        # A fresh boss per test, as bare as the object kitty hands over: the
        # registry may not exist yet.
        self.boss = types.SimpleNamespace()

    def test_a_registered_path_reaches_the_registry(self):
        answer = run(self.boss, "register", "/tmp/sub.sock")

        self.assertEqual(list(self.boss._agent_subs), ["/tmp/sub.sock"])
        self.assertIn("/tmp/sub.sock", answer)

    def test_an_unregistered_path_leaves_it(self):
        run(self.boss, "register", "/tmp/one.sock")
        run(self.boss, "register", "/tmp/two.sock")

        answer = run(self.boss, "unregister", "/tmp/one.sock")

        self.assertEqual(list(self.boss._agent_subs), ["/tmp/two.sock"])
        self.assertIn("/tmp/one.sock", answer)

    def test_the_registry_survives_between_calls(self):
        # `boss` is the same object across kitten invocations, which is what
        # makes a registration outlive the process that made it.
        run(self.boss, "register", "/tmp/one.sock")
        registry = self.boss._agent_subs

        run(self.boss, "register", "/tmp/two.sock")

        self.assertIs(self.boss._agent_subs, registry)
        self.assertEqual(list(registry), ["/tmp/one.sock", "/tmp/two.sock"])

    def test_registering_twice_is_one_registration(self):
        run(self.boss, "register", "/tmp/sub.sock")
        run(self.boss, "register", "/tmp/sub.sock")

        self.assertEqual(list(self.boss._agent_subs), ["/tmp/sub.sock"])

    def test_unregistering_a_path_nobody_registered_is_quiet(self):
        run(self.boss, "unregister", "/tmp/absent.sock")

        self.assertEqual(self.boss._agent_subs, {})

    def test_a_call_it_cannot_read_changes_nothing(self):
        cases = [
            ("no arguments", []),
            ("an action with no path", ["register"]),
            ("an unknown action", ["subscribe", "/tmp/sub.sock"]),
            ("one argument too many", ["register", "/tmp/sub.sock", "extra"]),
        ]
        for name, args in cases:
            with self.subTest(name):
                boss = types.SimpleNamespace()

                answer = run(boss, *args)

                self.assertNotEqual(answer, "")
                self.assertEqual(getattr(boss, "_agent_subs", {}), {})


if __name__ == "__main__":
    unittest.main()
