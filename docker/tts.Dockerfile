# Russian (Silero v5) + Kazakh (ISSAI KazakhTTS, ESPnet) TTS service. CPU by default; TORCH_INDEX=.../cu128 for CUDA.
FROM python:3.12-slim
ARG TORCH_INDEX=https://download.pytorch.org/whl/cpu
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg libsndfile1 curl build-essential && rm -rf /var/lib/apt/lists/*
WORKDIR /app/tts
COPY tts/requirements.txt .
RUN pip install --no-cache-dir torch --index-url ${TORCH_INDEX} \
 && pip install --no-cache-dir -r requirements.txt \
 && pip install --no-cache-dir --no-build-isolation parallel_wavegan
# bake the Silero Russian model (87 MB) into the image; Kazakh models go to the /models volume on first start
RUN python -c "import silero; silero.silero_tts(language='ru', speaker='v5_cis_base', sample_rate=24000); print('silero ok')"
COPY tts/ .
ENV TTS_MODELS_DIR=/models/tts TTS_HOST=0.0.0.0 TTS_PORT=9101 TTS_DEVICE=auto TTS_RU_DEVICE=cpu
EXPOSE 9101
HEALTHCHECK --interval=15s --timeout=5s --start-period=600s --retries=40 CMD curl -sf http://localhost:9101/health | grep -q '"status":"ok"' || exit 1
CMD ["sh", "-c", "python download_models.py --dir ${TTS_MODELS_DIR} && exec python server.py"]
