"""The gateway tallies the user's languages per conversation and passes the reply language to the router."""
import os
import sys
from pathlib import Path

os.environ["ROUTER_MODE"] = "mock"
os.environ["TTS_URL"] = "http://127.0.0.1:9"  # nothing listens there: TTS fails fast, the turn still completes
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from fastapi.testclient import TestClient  # noqa: E402

import server  # noqa: E402

client = TestClient(server.app)


def new_session() -> str:
    return client.post("/api/session").json()["session_id"]


def turn(sid: str, text: str, reply_language: str | None = None) -> dict:
    body = {"session_id": sid, "text": text}
    if reply_language:
        body["reply_language"] = reply_language
    r = client.post("/api/turn", json=body)
    assert r.status_code == 200, r.text
    return r.json()


def test_auto_follows_most_used_language_per_conversation():
    sid = new_session()
    assert turn(sid, "Сәлеметсіз бе, полис керек")["reply_lang"] == "kk"
    rec = turn(sid, "Мен көлік сақтандыруын қалаймын")
    assert rec["reply_lang"] == "kk" and rec["reply_lang_mode"] == "auto"
    # one Russian turn does not outweigh two Kazakh ones
    assert turn(sid, "Хочу оформить полис")["reply_lang"] == "kk"
    # a new conversation starts from scratch
    assert turn(new_session(), "Хочу оформить полис")["reply_lang"] == "ru"


def test_explicit_choice_overrides_usage():
    sid = new_session()
    rec = turn(sid, "Хочу оформить полис", reply_language="kk")
    assert rec["reply_lang"] == "kk" and rec["reply_lang_mode"] == "chosen"
