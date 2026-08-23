# Controller image: multi-stage build compiles the Go controller, then serves
# it with the pre-built frontend (if present in frontend/dist at build time).
FROM golang:1.25-alpine AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${AGENT_VERSION:-dev}" \
    -o /out/controller ./cmd/controller

FROM node:22-alpine AS frontend
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM alpine:3.20
RUN addgroup -S encode && adduser -S encode -G encode \
    && apk add --no-cache ca-certificates nfs-utils
WORKDIR /app
COPY --from=backend /out/controller /app/controller
COPY --from=frontend /src/dist /app/ui
COPY powershell/EncodeLib.ps1 /app/seed/EncodeLib.ps1

# Data dir: SQLite db, update payloads, UI. NFS shares mount under /data.
ENV ENCODE_DATA=/data
VOLUME /data
EXPOSE 8080
USER encode
ENTRYPOINT ["/app/controller"]
