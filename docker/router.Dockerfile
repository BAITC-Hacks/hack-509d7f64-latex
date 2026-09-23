# Layer-2 classifier/router (Go). Build context = repository root: needs backend/ and voice_router_dataset/
# (the catalog is embedded into the binary from ../voice_router_dataset via a local module replace).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY voice_router_dataset/ voice_router_dataset/
COPY backend/ backend/
WORKDIR /src/backend
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags='-s -w' -o /out/voice-router ./cmd/router

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -H app && mkdir -p /data && chown app /data
COPY --from=build /out/voice-router /usr/local/bin/voice-router
ENV LISTEN_ADDR=0.0.0.0:8080 DB_PATH=/data/voice_router.db
# SQLite state (sessions, turns, seeded mock backend) lives on this volume.
VOLUME /data
WORKDIR /data
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=5 CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
USER app
ENTRYPOINT ["voice-router"]
