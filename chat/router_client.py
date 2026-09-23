"""Dialogue router client for the voice chat gateway.

The transcript of each user turn goes to the Go router in `backend/` (layer 2), which picks the scenario,
runs the (synthetic) business actions and writes the reply. Its contract (see backend/README.md):

    POST ROUTER_URL                      # default http://127.0.0.1:8080/v1/turns
    {"session_id": "chat_ab12...", "request_id": "chat_ab12...-t3", "text": "<transcript>", "language": "ru|kk|mixed"}
    -> {"answer": "...", "language": "ru|kk", "status": "completed|awaiting_slot|awaiting_confirmation|clarification|cancelled|handoff",
        "active_scenario": "SC12", "pending_scenarios": [...],
        "trace": {"decision": {"scenarios": [{"scenario_id", "confidence", "reason"}], "alternatives": [...], "slots": {...}},
                  "actions": [...], "latency_ms": {...}, "error": "..."}}

Unknown fields are rejected (400), so only that payload is sent. `ROUTER_TOKEN` adds `Authorization: Bearer`
when the router runs with `API_TOKEN`. The response is flattened into the trace shape the frontend renders
(`language`, `scenarios`, `alternatives`, `reason`, plus `status`, `actions`, `latency_ms`, ...).

While no Go router is reachable, `ROUTER_MODE=auto` (default) falls back to a keyword mock built from
scenarios.json so the frontend can be exercised end to end; every mock reply is marked "source": "mock".
"""
from __future__ import annotations

import json
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

KAZAKH_LETTERS = frozenset("әғқңөұүһі")
REPLY_KEYS = ("answer", "reply", "response", "message", "text")


def detect_lang(text: str) -> str:
    words = text.split()
    kk = sum(1 for w in words if KAZAKH_LETTERS & set(w.lower()))
    if not words or kk == 0:
        return "ru"
    return "kk" if kk / len(words) >= 0.25 else "mixed"


def _tokens(text: str) -> set[str]:
    return {t for t in re.findall(r"[а-яёәғқңөұүһіa-z0-9]+", text.lower()) if len(t) > 2}


class MockRouter:
    """Keyword-overlap scenario picker over scenarios.json examples; replies with the scenario's opening line."""

    def __init__(self, dataset_dir: Path):
        self.scenarios: list[dict] = []
        self.system: dict[str, dict] = {}
        self.names: dict[str, str] = {}
        path = Path(dataset_dir) / "scenarios.json"
        if path.exists():
            data = json.loads(path.read_text(encoding="utf-8"))
            for sc in data.get("scenarios", []):
                ex = sc.get("examples") or []
                texts = [t for lst in ex.values() for t in lst] if isinstance(ex, dict) else list(ex)
                bag: set[str] = set()
                for t in texts:
                    bag |= _tokens(t if isinstance(t, str) else t.get("text", ""))
                self.scenarios.append({**sc, "_bag": bag})
                self.names[sc["scenario_id"]] = sc.get("name") or sc["scenario_id"]
            self.system = {s["id"]: s for s in data.get("system_intents", [])}
            self.names.update({k: v.get("name") or k for k, v in self.system.items()})

    def turn(self, text: str, lang: str) -> dict:
        t0 = time.perf_counter()
        reply_lang = "kk" if lang == "kk" else "ru"
        toks = _tokens(text)
        scored = sorted(((len(toks & sc["_bag"]), sc) for sc in self.scenarios), key=lambda x: -x[0])
        top = [(n, sc) for n, sc in scored[:3] if n > 0]
        if top and top[0][0] >= 2:
            n, sc = top[0]
            conf = round(min(0.95, 0.5 + 0.12 * n), 2)
            reply = ((sc.get("responses") or {}).get(reply_lang) or {}).get("opening") or ""
            reply = re.sub(r"\{[^}]+\}", "…", reply)
            scenarios = [{"scenario_id": sc["scenario_id"], "name": sc.get("name"), "confidence": conf}]
            alternatives = [{"scenario_id": s["scenario_id"], "confidence": round(0.3 + 0.1 * m, 2)} for m, s in top[1:]]
            reason = f"mock: {n} keyword matches with {sc['scenario_id']} examples"
        else:
            # the dataset's SYS_UNCLEAR line has {option_a}/{option_b} placeholders, so use a plain clarifying question
            reply = ("Не совсем поняла. Уточните, пожалуйста: вы хотите оформить полис, заявить о страховом случае или узнать статус?"
                     if reply_lang == "ru" else
                     "Дәл түсінбедім. Нақтылап айтыңызшы: полис рәсімдеу керек пе, сақтандыру жағдайы туралы хабарлау ма, әлде мәртебені білу ме?")
            scenarios = [{"scenario_id": "SYS_UNCLEAR", "name": "SYS_UNCLEAR", "confidence": 0.4}]
            alternatives = [{"scenario_id": s["scenario_id"], "confidence": 0.3} for _, s in top[:2]]
            reason = "mock: no scenario reached 2 keyword matches"
        return {"reply": reply, "lang": reply_lang, "source": "mock",
                "trace": {"language": lang, "scenarios": scenarios, "alternatives": alternatives, "reason": reason},
                "ms": round((time.perf_counter() - t0) * 1000)}


class Router:
    def __init__(self, url: str, session_url: str, mode: str, dataset_dir: Path, token: str = "", timeout: float = 70.0):
        self.url, self.session_url, self.mode, self.timeout = url.strip(), session_url.strip(), mode, timeout
        self.token = token.strip()
        self.mock = MockRouter(dataset_dir)
        self.last_error: str | None = None

    # --- remote helpers
    def _headers(self) -> dict[str, str]:
        h = {"Content-Type": "application/json; charset=utf-8"}
        if self.token:
            h["Authorization"] = f"Bearer {self.token}"
        return h

    def _post(self, url: str, payload: dict) -> dict:
        req = urllib.request.Request(url, data=json.dumps(payload, ensure_ascii=False).encode("utf-8"), method="POST",
                                     headers=self._headers())
        with urllib.request.urlopen(req, timeout=self.timeout) as r:
            return json.loads(r.read().decode() or "{}")

    def health_url(self) -> str:
        """The Go router answers GET /healthz on its root (no token needed)."""
        parts = urllib.parse.urlsplit(self.url)
        return urllib.parse.urlunsplit((parts.scheme, parts.netloc, "/healthz", "", ""))

    def reachable(self) -> bool:
        if not self.url:
            return False
        try:
            with urllib.request.urlopen(self.health_url(), timeout=2) as r:
                body = json.loads(r.read().decode() or "{}")
            if body.get("status") not in (None, "ok"):
                self.last_error = f"health: {body}"
                return False
            return True
        except urllib.error.HTTPError:
            return True  # server answered, just not that path
        except Exception as e:
            self.last_error = str(e)
            return False

    def status(self) -> dict:
        remote = bool(self.url) and self.mode != "mock"
        return {"mode": self.mode, "url": self.url or None, "remote_reachable": self.reachable() if remote else False,
                "mock_scenarios": len(self.mock.scenarios), "last_error": self.last_error}

    def new_session(self) -> str | None:
        """Ask the remote service for its own session id, if it has such an endpoint (the Go router does not: its
        sessions are created implicitly by the first turn, so this stays None and the chat session id is used)."""
        if self.mode == "mock" or not self.session_url:
            return None
        try:
            data = self._post(self.session_url, {})
            return data.get("session_id") or data.get("id")
        except Exception as e:
            self.last_error = f"session: {e}"
            return None

    # --- Go response -> frontend trace
    def _named(self, items: list) -> list[dict]:
        out = []
        for c in items or []:
            if isinstance(c, dict):
                sid = c.get("scenario_id")
                out.append({**c, "name": c.get("name") or self.mock.names.get(sid, "")})
        return out

    def _flatten(self, data: dict) -> dict:
        trace = data.get("trace") if isinstance(data.get("trace"), dict) else {}
        decision = trace.get("decision") if isinstance(trace.get("decision"), dict) else {}
        scenarios = self._named(decision.get("scenarios"))
        reasons = [f"{c.get('scenario_id')}: {c['reason']}" for c in scenarios if c.get("reason")]
        if not reasons and data.get("status"):
            reasons.append(f"status: {data['status']}")
        if trace.get("error"):
            reasons.append(f"router error: {trace['error']}")
        return {
            "language": data.get("language") or trace.get("language"),
            "scenarios": scenarios,
            "alternatives": self._named(decision.get("alternatives")),
            "reason": "; ".join(reasons) or "—",
            "status": data.get("status"),
            "active_scenario": data.get("active_scenario"),
            "pending_scenarios": data.get("pending_scenarios") or [],
            "slots": decision.get("slots") or {},
            "is_continuation": decision.get("is_continuation"),
            "needs_handoff": decision.get("needs_handoff"),
            "actions": [{"name": a.get("name"), "mode": a.get("mode"), "inputs": a.get("inputs"), "result": a.get("result")}
                        for a in trace.get("actions") or [] if isinstance(a, dict)],
            "latency_ms": trace.get("latency_ms") or {},
            "request_id": data.get("request_id"),
            "error": trace.get("error"),
        }

    def turn(self, session_id: str, remote_session: str | None, text: str, lang: str, turn: int) -> dict:
        if self.mode != "mock" and self.url:
            t0 = time.perf_counter()
            sid = remote_session or session_id
            try:
                data = self._post(self.url, {"session_id": sid, "request_id": f"{sid}-t{turn}", "text": text,
                                             "language": lang if lang in ("ru", "kk", "mixed") else detect_lang(text)})
                reply = next((data[k] for k in REPLY_KEYS if isinstance(data.get(k), str) and data[k].strip()), None)
                if reply is None:
                    raise ValueError(f"no reply text in response keys {sorted(data)[:8]}")
                trace = self._flatten(data)
                return {"reply": reply, "lang": trace.get("language") or detect_lang(reply), "source": "remote",
                        "status": data.get("status"), "trace": trace, "ms": round((time.perf_counter() - t0) * 1000)}
            except urllib.error.HTTPError as e:
                body = e.read().decode(errors="replace")[:300]
                self.last_error = f"HTTP {e.code}: {body}"
                if self.mode == "remote":
                    raise RuntimeError(f"router HTTP {e.code}: {body}")
            except Exception as e:
                self.last_error = str(e)
                if self.mode == "remote":
                    raise RuntimeError(f"router unreachable at {self.url}: {e}")
            fallback = self.mock.turn(text, lang)
            fallback["source"] = "mock"
            fallback["trace"]["reason"] += f" (remote router failed: {self.last_error})"
            return fallback
        return self.mock.turn(text, lang)
