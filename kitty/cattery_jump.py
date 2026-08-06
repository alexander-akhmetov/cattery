"""
cattery_jump: focus the next attention-worthy agent window.

Walks every kitty window in every OS window, picks the most urgent one by
AGENT_DISPLAY (blocked before done), and focuses it, switching OS window if
needed. It does nothing when no agent window needs attention.

Unbound by default. Bind with `map kitty_mod+a>j kitten cattery_jump.py`.
"""

import sys

from kitty.boss import Boss
from kittens.tui.handler import result_handler

# The lower rank wins. "working" and "idle" are missing, because jumping to a
# working agent is rarely useful and the user already dismissed the idle ones.
_URGENCY = {"blocked": 0, "done": 1}


def main(args: list[str]) -> str:
    # No-UI mode: handle_result runs without a call to main().
    return ""


@result_handler(no_ui=True)
def handle_result(args: list[str], answer: str, target_window_id: int, boss: Boss) -> None:
    candidates = []
    for w in boss.window_id_map.values():
        rank = _URGENCY.get(w.user_vars.get("AGENT_DISPLAY"))
        if rank is None:
            continue
        # Skip the current window so the keybinding cycles to the next agent.
        if w.id == target_window_id:
            continue
        candidates.append((rank, w.id, w))
    if not candidates:
        return
    candidates.sort()
    target = candidates[0][2]
    boss.set_active_window(target, switch_os_window_if_needed=True)


if __name__ == "__main__":
    main(sys.argv)
