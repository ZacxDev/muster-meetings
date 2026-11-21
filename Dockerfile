# syntax=docker/dockerfile:1
#
# muster-meetings: one static Go binary (modernc SQLite is pure Go, so
# CGO_ENABLED=0) with the pages, scripts and the vendored LiveKit client
# embedded (internal/server/web, //go:embed), on distroless as nonroot.
#
# The root filesystem can be mounted read-only: the only writes are the
# SQLite database under STATE_DIR (/state, a volume) and SQLite's temporary
# files (mount a tmpfs or emptyDir at /tmp; see deploy/kubernetes and
# compose.meetings.yml).
#
# Base images are pinned by digest; bump the tag and the digest together.

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/muster-meetings . \
 && mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/muster-meetings /muster-meetings
# An empty /state owned by nonroot, so a fresh named volume (Compose) starts
# writable; Kubernetes sets fsGroup on its PVC instead.
COPY --from=build --chown=65532:65532 /out/state /state
ENV STATE_DIR=/state \
    MEETINGS_LISTEN=:8080 \
    MEETINGS_INTERNAL_LISTEN=:8081
VOLUME ["/state"]
USER 65532:65532
# 8080: the public listener (behind your HTTPS proxy). 8081: internal only
# (/healthz, LiveKit's webhook); never route it publicly.
EXPOSE 8080 8081
ENTRYPOINT ["/muster-meetings"]
