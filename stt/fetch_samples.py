"""Fetch a few FLEURS (CC-BY-4.0) Kazakh and Russian test clips with reference transcripts into stt/samples/.

    python stt/fetch_samples.py [--per-lang 5]
"""
import argparse
import json
import urllib.parse
import urllib.request
from pathlib import Path

SAMPLES = Path(__file__).resolve().parent / "samples"
API = "https://datasets-server.huggingface.co/first-rows?"


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--per-lang", type=int, default=5)
    args = ap.parse_args()
    SAMPLES.mkdir(exist_ok=True)
    manifest = []
    for cfg, lang in (("kk_kz", "kk"), ("ru_ru", "ru")):
        url = API + urllib.parse.urlencode(dict(dataset="mteb/fleurs", config=cfg, split="test"))
        rows = json.load(urllib.request.urlopen(url, timeout=60))["rows"]
        for r in rows[:args.per_lang]:
            row = r["row"]
            fn = SAMPLES / f"fleurs_{lang}_{row['id']}.wav"
            if not fn.exists():
                urllib.request.urlretrieve(row["audio"][0]["src"], fn)
            manifest.append(dict(file=fn.name, lang=lang,
                                 source=f"google/fleurs {cfg} test id={row['id']} (via mteb/fleurs)",
                                 transcription=row["transcription"], raw_transcription=row["raw_transcription"],
                                 num_samples=row["num_samples"]))
            print(f"{fn.name}  {row['num_samples'] / 16000:.1f}s  {row['transcription'][:60]}")
    (SAMPLES / "manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"wrote {SAMPLES / 'manifest.json'} ({len(manifest)} clips)")


if __name__ == "__main__":
    main()
