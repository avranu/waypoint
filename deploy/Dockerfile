# Waypoint: one static binary with the web app embedded.
# Build from the repo root:  docker build -f deploy/Dockerfile -t waypoint .
# (or `make docker` / `make deploy`). Base images float on patch releases;
# Renovate proposes digest pins (renovate.json).

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY web/ ./
# Fail here, not at runtime, if the build produced no app to embed.
RUN npm run build && test -s dist/index.html

FROM golang:1.27-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go ./web/
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /waypoint ./cmd/waypoint
# The wp CLI for every platform, served at /cli (curl -fsSL <server>/cli | sh).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    sh cmd/wp/build-all.sh /out/cli "${VERSION}"
# Distroless has no shell, so prepare a /data the nonroot user can write.
RUN mkdir -p /out/data/backups && chmod 0750 /out/data /out/data/backups

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="Waypoint" \
      org.opencontainers.image.description="Self-hosted issue tracker" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=server /waypoint /waypoint
COPY --from=server /out/cli /cli
COPY --from=server --chown=65532:65532 /out/data /data
ENV WAYPOINT_ADDR=:8080 \
    WAYPOINT_DB=/data/waypoint.db \
    WAYPOINT_CLI_DIR=/cli \
    WAYPOINT_BACKUP_DIR=/data/backups
VOLUME /data
EXPOSE 8080
USER nonroot
HEALTHCHECK --interval=30s --timeout=6s --start-period=10s CMD ["/waypoint", "healthcheck"]
ENTRYPOINT ["/waypoint"]
CMD ["serve"]
