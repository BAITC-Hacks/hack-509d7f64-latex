"""Which language the agent answers in.

The browser sends a preference with every turn: "ru" / "kk" (picked explicitly) or "auto". In auto mode the reply
follows the language this user has spoken most in the current conversation; mixed Russian-Kazakh turns count for
neither side, and a tie goes to the latest non-mixed turn. With nothing to go on (only mixed turns so far) the
choice is left to the router, which picks the dominant language itself.
"""
from __future__ import annotations

LANGS = ("ru", "kk")
PREFS = ("auto", *LANGS)


def normalize_pref(value) -> str:
    return value if value in PREFS else "auto"


def new_tally() -> dict:
    return {"counts": {lang: 0 for lang in LANGS}, "last": None}


def observe(tally: dict, lang: str | None) -> None:
    """Record the language of one user turn."""
    if lang in LANGS:
        tally["counts"][lang] += 1
        tally["last"] = lang


def choose_reply_lang(tally: dict, pref: str) -> tuple[str | None, str]:
    """-> (reply_language or None, how it was decided: "chosen" | "auto" | "router")."""
    pref = normalize_pref(pref)
    if pref != "auto":
        return pref, "chosen"
    ru, kk = tally["counts"]["ru"], tally["counts"]["kk"]
    if ru == kk:
        return (tally["last"], "auto") if tally["last"] else (None, "router")
    return ("ru" if ru > kk else "kk"), "auto"
