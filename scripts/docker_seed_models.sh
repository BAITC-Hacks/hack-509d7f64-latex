#!/usr/bin/env bash
# Copy already-downloaded weights from this checkout into the Docker "speech-models" volume, so the
# containers start without downloading anything (also the fix when Hugging Face is unreachable from Docker).
#   scripts/docker_seed_models.sh
set -euo pipefail
cd "$(dirname "$0")/.."
VOL=speech-models   # fixed volume name, see docker-compose.yml
docker volume create "$VOL" >/dev/null
seed() {  # src dir, dest name
  [[ -d "$1" ]] || { echo "skip $1 (not present; run the download script first)"; return; }
  echo "seeding $1 -> volume $VOL:/models/$2"
  docker run --rm -v "$PWD/$1:/src:ro" -v "$VOL:/models" alpine sh -c "mkdir -p /models/$2 && cp -a /src/. /models/$2/ && du -sh /models/$2"
}
seed stt/models stt
seed tts/models tts
