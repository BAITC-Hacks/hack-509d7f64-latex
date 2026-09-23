# Voice chat frontend + gateway (browser <-> STT / router / TTS)
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends libsndfile1 curl && rm -rf /var/lib/apt/lists/*
WORKDIR /app/chat
COPY chat/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY chat/ .
COPY voice_router_dataset/ /app/voice_router_dataset/
ENV CHAT_HOST=0.0.0.0 CHAT_PORT=9102 STT_URL=http://stt:9100 TTS_URL=http://tts:9101 \
    DATASET_DIR=/app/voice_router_dataset ROUTER_MODE=auto
EXPOSE 9102
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=5 CMD curl -sf http://localhost:9102/health || exit 1
CMD ["python", "server.py"]
