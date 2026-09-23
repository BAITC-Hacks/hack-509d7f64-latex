"""Text normalization and WER/CER, matching the model card's evaluation protocol:
lowercase, punctuation stripped, ё -> е, the non-lexical "_" marker removed."""
import re
import unicodedata

_PUNCT = re.compile(r"[^\w\s-]|_", re.UNICODE)


def normalize(text: str) -> str:
    text = unicodedata.normalize("NFC", text).lower().replace("ё", "е")
    text = _PUNCT.sub(" ", text)
    return re.sub(r"\s+", " ", text).strip()


def _edit_distance(ref: list, hyp: list) -> int:
    prev = list(range(len(hyp) + 1))
    for i, r in enumerate(ref, 1):
        cur = [i] + [0] * len(hyp)
        for j, h in enumerate(hyp, 1):
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (r != h))
        prev = cur
    return prev[-1]


def wer(reference: str, hypothesis: str) -> float:
    ref, hyp = normalize(reference).split(), normalize(hypothesis).split()
    return _edit_distance(ref, hyp) / max(len(ref), 1)


def cer(reference: str, hypothesis: str) -> float:
    ref, hyp = list(normalize(reference)), list(normalize(hypothesis))
    return _edit_distance(ref, hyp) / max(len(ref), 1)


def align(reference: str, hypothesis: str) -> list[dict]:
    """Word-level alignment as a list of {"op": eq|sub|del|ins, "ref": ..., "hyp": ...}."""
    ref, hyp = normalize(reference).split(), normalize(hypothesis).split()
    n, m = len(ref), len(hyp)
    d = [[0] * (m + 1) for _ in range(n + 1)]
    for i in range(1, n + 1):
        d[i][0] = i
    for j in range(1, m + 1):
        d[0][j] = j
    for i in range(1, n + 1):
        for j in range(1, m + 1):
            d[i][j] = min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + (ref[i - 1] != hyp[j - 1]))
    ops: list[dict] = []
    i, j = n, m
    while i > 0 or j > 0:
        if i > 0 and j > 0 and d[i][j] == d[i - 1][j - 1] + (ref[i - 1] != hyp[j - 1]):
            ops.append({"op": "eq" if ref[i - 1] == hyp[j - 1] else "sub", "ref": ref[i - 1], "hyp": hyp[j - 1]})
            i, j = i - 1, j - 1
        elif i > 0 and d[i][j] == d[i - 1][j] + 1:
            ops.append({"op": "del", "ref": ref[i - 1], "hyp": None})
            i -= 1
        else:
            ops.append({"op": "ins", "ref": None, "hyp": hyp[j - 1]})
            j -= 1
    return ops[::-1]
