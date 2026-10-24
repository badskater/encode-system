# Controller image: multi-stage build compiles the Go controller, then serves
# it with the pre-built frontend (if present in frontend/dist at build time).
FROM golang:1.25-alpine AS backend
ARG AGENT_VERSION=dev
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${AGENT_VERSION}" \
    -o /out/controller ./cmd/controller

FROM node:22-alpine AS frontend
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM alpine:3.20
RUN addgroup -S encode && adduser -S encode -G encode \
    && apk add --no-cache ca-certificates nfs-utils wget \
    && mkdir -p /data && chown encode:encode /data
WORKDIR /app
COPY --from=backend /out/controller /app/controller
COPY --from=frontend /src/dist /app/ui
COPY powershell/EncodeLib.ps1 /app/seed/EncodeLib.ps1
# Provision tree (PlaybookDir=/app/provision, set below):
#   provision.yml (Windows executeWindows reads PlaybookDir/provision.yml)
#   linux-node.yml + roles/linux-node/ (executeLinux reads PlaybookDir/
#   linux-node.yml and copies PlaybookDir/roles/linux-node/ into the run dir)
COPY infra/ansible/provision.yml /app/provision/provision.yml
COPY infra/ansible/linux-node.yml /app/provision/linux-node.yml
COPY infra/ansible/roles/linux-node /app/provision/roles/linux-node
# systemd unit: stageSystemdUnit resolves PlaybookDir/../systemd/encode-agent.service
# FIRST — /app/provision/../systemd → /app/systemd. Ships as sibling of provision/.
COPY infra/systemd/encode-agent.service /app/systemd/encode-agent.service

# Data dir: SQLite db + update payloads (persistent). The SPA is baked into
# /app/ui and served via ENCODE_UI_DIR, so it stays out of the data volume.
# /data must be owned by the runtime user BEFORE VOLUME: fresh named volumes
# inherit the image dir's ownership, and a root-owned /data makes the SQLite
# open fail with a misleading "out of memory (14)".
ENV ENCODE_DATA=/data
ENV ENCODE_UI_DIR=/app/ui
ENV ENCODE_PROVISION_DIR=/app/provision
VOLUME /data
EXPOSE 8080
USER encode
ENTRYPOINT ["/app/controller"]
