"""Transcribe audio files from the command line.

    python stt/transcribe.py audio.wav [more.mp3 ...] [--lang rukk|kk|ru] [--device auto|cpu|cuda] [--no-vad] [--json]
"""
import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from engine import STTEngine  # noqa: E402


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("audio", nargs="+", help="audio file(s); any format ffmpeg can read")
    ap.add_argument("--lang", default="rukk", choices=["rukk", "kk", "ru"])
    ap.add_argument("--device", default="auto")
    ap.add_argument("--no-vad", action="store_true", help="skip VAD, use fixed 20 s windows")
    ap.add_argument("--json", action="store_true", help="print full JSON (segments, timings)")
    args = ap.parse_args()

    engine = STTEngine(lang=args.lang, device=args.device)
    print(f"[model={args.lang} device={engine.device}]", file=sys.stderr)
    for path in args.audio:
        result = engine.transcribe(path, use_vad=not args.no_vad)
        if args.json:
            print(json.dumps({"file": path, **result.to_dict()}, ensure_ascii=False, indent=2))
        else:
            print(f"{path}  [{result.duration_s}s audio, {result.timings_ms['total']} ms, lang~{result.language_hint}]")
            print(f"  {result.text}")


if __name__ == "__main__":
    main()
