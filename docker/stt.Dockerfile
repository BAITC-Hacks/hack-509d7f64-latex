# Kazakh/Russian STT service (+ test console). CPU by default; build with TORCH_INDEX=https://download.pytorch.org/whl/cu128 for CUDA.
FROM python:3.12-slim
ARG TORCH_INDEX=https://download.pytorch.org/whl/cpu
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg libsndfile1 curl && rm -rf /var/lib/apt/lists/*
WORKDIR /app/stt
COPY stt/requirements.txt .
RUN pip install --no-cache-dir torch --index-url ${TORCH_INDEX} && pip install --no-cache-dir -r requirements.txt
COPY stt/ .
COPY voice_router_dataset/ /app/voice_router_dataset/
ENV STT_MODELS_DIR=/models/stt STT_HOST=0.0.0.0 STT_PORT=9100 STT_UI=1 \
    STT_DATASET_DIR=/app/voice_router_dataset TTS_URL=http://tts:9101
EXPOSE 9100
HEALTHCHECK --interval=15s --timeout=5s --start-period=600s --retries=40 CMD curl -sf http://localhost:9100/health | grep -q '"status":"ok"' || exit 1
# weights (756 MB) are fetched into the /models volume on first start, then reused
CMD ["sh", "-c", "python download_model.py --dir ${STT_MODELS_DIR} && exec python server.py"]
