"""Download the STT model files from Hugging Face into stt/models/ (idempotent).

    python stt/download_model.py                 # mixed kk+ru acoustic model (721 MB) + VAD
    python stt/download_model.py --lang kk --lang ru   # also the monolingual models (360 MB each)
    python stt/download_model.py --with-lm       # + KenLM language model for beam search (3+ GB, optional)

Model: https://huggingface.co/alibiserikbay/kazakh-russian-mixed-stt (Apache-2.0)
"""
import argparse
from pathlib import Path

from huggingface_hub import snapshot_download

REPO = "alibiserikbay/kazakh-russian-mixed-stt"
MODELS_DIR = Path(__file__).resolve().parent / "models"


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--lang", action="append", choices=["rukk", "kk", "ru"],
                    help="acoustic model(s) to fetch; default: rukk (mixed Kazakh+Russian)")
    ap.add_argument("--with-lm", action="store_true", help="also fetch the KenLM files for beam-search decoding")
    ap.add_argument("--dir", type=Path, default=MODELS_DIR, help=f"target directory (default: {MODELS_DIR})")
    args = ap.parse_args()

    langs = args.lang or ["rukk"]
    patterns = ["config.json", "vad/*", "benchmarks/*"] + [f"asr/{lang}/*" for lang in langs]
    if args.with_lm:
        patterns += [f"lm/{lang}/*" for lang in langs]

    print(f"Downloading {patterns} from {REPO} -> {args.dir}")
    path = snapshot_download(REPO, allow_patterns=patterns, local_dir=str(args.dir))
    for lang in langs:
        model = Path(path) / "asr" / lang / "model.pt"
        print(f"  asr/{lang}/model.pt: {model.stat().st_size / 1e6:.0f} MB" if model.exists() else f"  asr/{lang}/model.pt: MISSING")
    print("done")


if __name__ == "__main__":
    main()
