"""
cattery_events: add and remove agent-event subscribers.

The watcher sends one datagram per agent state transition to every socket path
in `boss._agent_subs`. That registry is a dict inside kitty's own process, so
nothing outside kitty can reach it, and this kitten is the way in:

    kitten @ kitten /path/to/cattery_events.py register /path/to/sub.sock
    kitten @ kitten /path/to/cattery_events.py unregister /path/to/sub.sock

`cattery events` runs both, around a socket it binds itself. The confirmation
string comes back on the caller's stdout.

A subscriber that dies without unregistering leaks nothing lasting: the watcher
drops a path whose socket answers ENOENT or ECONNREFUSED.
"""

import sys

from kitty.boss import Boss
from kittens.tui.handler import result_handler


def main(args: list[str]) -> str:
    # No-UI mode: handle_result runs without a call to main().
    return ""


@result_handler(no_ui=True)
def handle_result(args: list[str], answer: str, target_window_id: int, boss: Boss) -> str:
    # kitty puts this kitten's own path in args[0].
    if len(args) != 3:
        return "usage: cattery_events.py <register|unregister> <socket path>"
    action, path = args[1], args[2]
    # The watcher creates the registry too, and either can run first: a
    # subscriber can start before any agent has published a state.
    if not hasattr(boss, "_agent_subs"):
        boss._agent_subs = {}
    if action == "register":
        boss._agent_subs[path] = None
        return "registered " + path
    if action == "unregister":
        boss._agent_subs.pop(path, None)
        return "unregistered " + path
    return "unknown action " + action


if __name__ == "__main__":
    main(sys.argv)
