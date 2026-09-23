"""Reply-language choice: explicit picker vs. the conversation's most-used language, and the hand-off to the router."""
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(HERE))

from reply_lang import choose_reply_lang, new_tally, normalize_pref, observe  # noqa: E402
from router_client import MockRouter, Router  # noqa: E402

DATASET = HERE.parent / "voice_router_dataset"


def test_normalize_pref_accepts_only_known_values():
    assert normalize_pref("ru") == "ru"
    assert normalize_pref("kk") == "kk"
    assert normalize_pref("auto") == "auto"
    assert normalize_pref(None) == "auto"
    assert normalize_pref("en") == "auto"


def test_explicit_choice_wins_over_usage():
    t = new_tally()
    for _ in range(3):
        observe(t, "kk")
    assert choose_reply_lang(t, "ru") == ("ru", "chosen")


def test_auto_uses_most_used_language_in_conversation():
    t = new_tally()
    observe(t, "kk")
    observe(t, "kk")
    observe(t, "ru")
    assert choose_reply_lang(t, "auto") == ("kk", "auto")


def test_auto_tie_uses_latest_non_mixed_turn():
    t = new_tally()
    observe(t, "ru")
    observe(t, "kk")
    observe(t, "mixed")
    assert choose_reply_lang(t, "auto") == ("kk", "auto")


def test_mixed_turns_do_not_count():
    t = new_tally()
    observe(t, "mixed")
    observe(t, "mixed")
    observe(t, "ru")
    assert t["counts"] == {"ru": 1, "kk": 0}
    assert choose_reply_lang(t, "auto") == ("ru", "auto")


def test_auto_with_only_mixed_input_leaves_it_to_the_router():
    t = new_tally()
    observe(t, "mixed")
    assert choose_reply_lang(t, "auto") == (None, "router")


def test_router_sends_reply_language_to_go_router():
    r = Router("http://router.test/v1/turns", "", "remote", DATASET)
    sent = {}

    def fake_post(url, payload):
        sent.update(payload)
        return {"answer": "Сәлем", "language": "kk", "status": "completed", "trace": {}}

    r._post = fake_post
    out = r.turn("chat_x", None, "привет", "ru", 1, reply_lang="kk")
    assert sent["reply_language"] == "kk"
    assert out["lang"] == "kk"


def test_router_omits_reply_language_when_not_chosen():
    r = Router("http://router.test/v1/turns", "", "remote", DATASET)
    sent = {}
    r._post = lambda url, payload: sent.update(payload) or {"answer": "ok", "trace": {}}
    r.turn("chat_x", None, "привет", "ru", 1)
    assert "reply_language" not in sent


def test_mock_router_answers_in_requested_language():
    m = MockRouter(DATASET)
    assert m.turn("непонятно что", "ru", reply_lang="kk")["lang"] == "kk"
    assert m.turn("непонятно что", "kk", reply_lang="ru")["lang"] == "ru"
