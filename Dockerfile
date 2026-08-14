# One image, every role. `kiln serve`, `kiln worker`, and `kiln admin migrate`
# are subcommands of a single static binary, so an operator deploys the same
# artifact three ways rather than tracking three images that must stay in
# lockstep. The reading UI is embedded in the binary; there is nothing else to
# serve or mount.
#
# The runtime is debian-slim rather than distroless because the worker shells
# out to real programs: git (repository sync), pdftotext and pdfimages (PDF
# text and figures), and pandoc (Office/HTML text and embedded media). A
# scratch image would build fine and then fail at the first PDF.

# --- build ------------------------------------------------------------------
FROM golang:1.25-bookworm AS build
WORKDIR /src

# Dependencies resolve in their own layer so source edits do not re-download
# the module graph on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is injected at link time into the same symbol the Makefile and
# GoReleaser use, so `kiln version`, GET /api/v1/version, and the UI's skew
# check all report the real tag.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X github.com/daiwa-zou/kiln/internal/observability.Version=${VERSION}" \
      -o /out/kiln ./cmd/kiln

# --- runtime ----------------------------------------------------------------
FROM debian:bookworm-slim AS runtime

# ca-certificates: HTTPS to the Anthropic API, GitHub, and web sources.
# git: repository connectors. poppler-utils: pdftotext and pdfimages, which
# ship together. pandoc: Office/HTML text and --extract-media for figures.
# tini: PID 1 that reaps zombies and forwards SIGTERM, which the worker's
# drain-with-grace shutdown depends on.
RUN apt-get update \
    && apt-get install --no-install-recommends -y \
        ca-certificates \
        git \
        pandoc \
        poppler-utils \
        tini \
    && rm -rf /var/lib/apt/lists/*

# Unprivileged by construction: a fixed uid/gid so Kubernetes can assert
# runAsUser without depending on image internals, and so a mounted volume's
# ownership is predictable.
RUN groupadd --gid 65532 kiln \
    && useradd --uid 65532 --gid 65532 --home-dir /home/kiln --create-home kiln \
    && mkdir -p /var/lib/kiln/blobs \
    && chown -R 65532:65532 /var/lib/kiln

COPY --from=build /out/kiln /usr/local/bin/kiln

USER 65532:65532
WORKDIR /home/kiln

# The fs blob backend's default location, declared so a single-node deployment
# that has not configured S3 does not silently store uploads on a container
# layer that vanishes with the pod.
VOLUME ["/var/lib/kiln/blobs"]

EXPOSE 8080

# Config comes from the environment (every key binds as KILN_*), so the image
# ships no config file and needs no entrypoint script.
ENV KILN_HTTP_ADDR=:8080

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/kiln"]
CMD ["serve"]

LABEL org.opencontainers.image.title="kiln" \
      org.opencontainers.image.description="Self-hosted platform that compiles and maintains knowledge-base wikis from code, documents, and web pages." \
      org.opencontainers.image.source="https://github.com/daiwa-zou/kiln" \
      org.opencontainers.image.licenses="Apache-2.0"
