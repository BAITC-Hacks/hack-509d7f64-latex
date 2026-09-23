"""Transcribe the bundled FLEURS samples (stt/samples/) and report WER / CER per file and per language.

    python stt/eval_samples.py [--lang rukk] [--device auto|cpu|cuda]
"""
import argparse
import json
import sys
from collections import defaultdict
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from engine import STTEngine  # noqa: E402
from metrics import cer, wer  # noqa: E402

SAMPLES = Path(__file__).resolve().parent / "samples"


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--lang", default="rukk", choices=["rukk", "kk", "ru"])
    ap.add_argument("--device", default="auto")
    ap.add_argument("--no-vad", action="store_true")
    args = ap.parse_args()

    manifest = json.loads((SAMPLES / "manifest.json").read_text(encoding="utf-8"))
    manifest = [m for m in manifest if (SAMPLES / m["file"]).exists()]
    if not manifest:
        sys.exit("no samples found; run: python stt/fetch_samples.py")

    engine = STTEngine(lang=args.lang, device=args.device)
    print(f"model={args.lang} device={engine.device} samples={len(manifest)}\n")
    per_lang = defaultdict(list)
    for m in manifest:
        r = engine.transcribe(SAMPLES / m["file"], use_vad=not args.no_vad)
        w, c = wer(m["transcription"], r.text), cer(m["transcription"], r.text)
        per_lang[m["lang"]].append((w, c, r.timings_ms["total"], r.duration_s))
        print(f"{m['file']:<24} {m['lang']}  WER {w:6.1%}  CER {c:6.1%}  {r.timings_ms['total']:>5} ms / {r.duration_s:>5.1f} s")
        print(f"   ref: {m['transcription']}")
        print(f"   hyp: {r.text}\n")

    print(f"{'lang':<6}{'n':>4}{'WER':>9}{'CER':>9}{'xRT':>8}")
    for lang, rows in sorted(per_lang.items()):
        n = len(rows)
        proc = sum(r[2] for r in rows) / 1000
        audio = sum(r[3] for r in rows)
        print(f"{lang:<6}{n:>4}{sum(r[0] for r in rows) / n:>9.1%}{sum(r[1] for r in rows) / n:>9.1%}{audio / proc:>8.1f}")


if __name__ == "__main__":
    main()
