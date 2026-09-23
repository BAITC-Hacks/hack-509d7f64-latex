"""Download the ISSAI KazakhTTS Tacotron2 models and ParallelWaveGAN vocoders into tts/models/kazakh/<voice>/.
The Silero Russian model is fetched automatically by the `silero` package on first use.

    python tts/download_models.py                       # female1 + male1 (~245 MB)
    python tts/download_models.py --voice female2 --voice female3
"""
import argparse
import os
import urllib.request
import zipfile
from pathlib import Path

BASE = "https://issai.nu.edu.kz/wp-content/uploads/2022/03/"
MODELS_DIR = Path(os.environ.get("TTS_MODELS_DIR", Path(__file__).resolve().parent / "models")) / "kazakh"
VOICES = ["female1", "female2", "female3", "male1", "male2"]


def fetch(voice: str, models_dir: Path = MODELS_DIR) -> None:
    target = models_dir / voice
    if (target / "tts" / "exp" / "tts_train_raw_char" / "config.yaml").exists() and (target / "vocoder" / "config.yml").exists():
        print(f"{voice}: already present")
        return
    for name, sub in ((f"kaztts_{voice}_tacotron2_train.loss.ave.zip", "tts"), (f"parallelwavegan_{voice}_checkpoint.zip", "vocoder")):
        zpath = target / name
        (target / sub).mkdir(parents=True, exist_ok=True)
        print(f"{voice}: downloading {name} ...")
        urllib.request.urlretrieve(BASE + name, zpath)
        with zipfile.ZipFile(zpath) as z:
            z.extractall(target / sub, members=[m for m in z.namelist() if "/images/" not in m])
        zpath.unlink()
    print(f"{voice}: done -> {target}")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--voice", action="append", choices=VOICES, help="default: female1 and male1")
    ap.add_argument("--dir", type=Path, default=None, help=f"models root (default: $TTS_MODELS_DIR or tts/models); voices go under <dir>/kazakh/")
    args = ap.parse_args()
    models_dir = (args.dir / "kazakh") if args.dir else MODELS_DIR
    for v in args.voice or ["female1", "male1"]:
        fetch(v, models_dir)


if __name__ == "__main__":
    main()
