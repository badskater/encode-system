# Controller image: multi-stage build compiles the Go controller, then serves
# it with the pre-built frontend (if present in frontend/dist at build time).
FROM golang:1.25-alpine AS backend
ARG AGENT_VERSION=dev
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${AGENT_VERSION}" \
    -o /out/controller ./cmd/controller \
    && CGO_ENABLED=0 go build -ldflags "-X main.Version=${AGENT_VERSION}" \
    -o /out/pgimport ./cmd/pgimport

FROM node:22-alpine AS frontend
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM alpine:3.20
# Provisioning capability: the GHCR image is the production controller for
# installs that can reach ghcr.io (install.sh pulls it first, falling back to
# building Dockerfile.runtime). The provision engine runs ansible-playbook
# INSIDE this container (engine.go executeWindows/executeLinux), so the image
# must carry ansible-core + both collections: ansible.windows for WinRM
# provisioning (provision.yml) and ansible.posix for roles/linux-node's
# ansible.posix.mount tasks. Alpine's apk ansible package is unmaintained and
# far behind ansible-core 2.21, so install via pip (same proven pattern as
# Dockerfile.runtime). Versions are pinned for the same fleet-stability reason
# documented there — an unpinned rebuild could pull a breaking release.
RUN addgroup -S encode && adduser -S encode -G encode \
    && apk add --no-cache ca-certificates nfs-utils wget \
        python3 py3-pip \
    && pip3 install --break-system-packages --no-cache-dir \
        'ansible-core==2.21.3' 'pywinrm==0.5.0' \
    && ansible-galaxy collection install ansible.windows:3.7.0 ansible.posix:2.2.2 \
    && rm -rf /root/.cache /tmp/* \
    && mkdir -p /data && chown encode:encode /data
WORKDIR /app
COPY --from=backend /out/controller /app/controller
COPY --from=backend /out/pgimport /app/pgimport
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
