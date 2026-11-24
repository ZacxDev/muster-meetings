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
#
# ffmpeg (frame sampling, internal/frames): a MINIMAL static build of FFmpeg
# 7.1.5, the release tarball pinned by SHA-256 (it verifies against the
# FFmpeg release signing key, FCF9 86EA 15E6 E293 A564 4F10 B432 2F04 D676
# 58D8). Only what sampling a frame from Egress's MP4 needs: the file, pipe,
# tcp and http protocols (it reads the recording over a loopback HTTP range
# proxy, never a TLS endpoint), the mov demuxer, the H.264 decoder (Egress
# records H.264) and VP9 (the tests' fixture), the scale and format filters,
# and the MJPEG encoder to image2pipe. No external library, no device, no
# ffprobe. It is cross-compiled on the build platform, so the arm64 image
# needs no emulation; a frame is extracted from the H.264 fixture to prove
# the amd64 binary works before it is copied.

FROM --platform=$BUILDPLATFORM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e AS ffmpeg
ARG TARGETARCH
ARG FFMPEG_VERSION=7.1.5
ARG FFMPEG_SHA256=de668509caf9e35e3cd162473441fdb29538c6d96ed080292b3cf9e6fc5d558f
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils make pkg-config nasm gcc libc6-dev \
      gcc-aarch64-linux-gnu libc6-dev-arm64-cross binutils-aarch64-linux-gnu \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /ffmpeg
RUN curl -sSfL -o ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz" \
 && echo "${FFMPEG_SHA256}  ffmpeg.tar.xz" | sha256sum -c - \
 && tar -xJf ffmpeg.tar.xz --strip-components=1 && rm ffmpeg.tar.xz
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) cross="" ;; \
      arm64) cross="--enable-cross-compile --arch=aarch64 --target-os=linux --cross-prefix=aarch64-linux-gnu-" ;; \
      *) echo "no ffmpeg build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    ./configure $cross --prefix=/out --enable-static --disable-shared --pkg-config-flags=--static --extra-ldflags=-static \
      --disable-autodetect --disable-everything --disable-doc --disable-debug --disable-ffplay --disable-ffprobe \
      --disable-avdevice --disable-swresample \
      --enable-protocol=file,pipe,tcp,http --enable-demuxer=mov --enable-parser=h264,vp9 \
      --enable-decoder=h264,vp9 --enable-encoder=mjpeg --enable-muxer=image2pipe \
      --enable-filter=scale,format,null,buffer,buffersink --enable-swscale; \
    make -j"$(nproc)"; make install; \
    case "$TARGETARCH" in amd64) strip /out/bin/ffmpeg ;; arm64) aarch64-linux-gnu-strip /out/bin/ffmpeg ;; esac
COPY internal/frames/testdata/recording-h264.mp4 /tmp/recording-h264.mp4
RUN if [ "$TARGETARCH" = amd64 ]; then \
      /out/bin/ffmpeg -nostdin -hide_banner -loglevel error -ss 1.5 -i /tmp/recording-h264.mp4 -frames:v 1 \
        -vf "scale='min(1280,iw)':-2" -f image2pipe -c:v mjpeg -q:v 3 pipe:1 > /tmp/frame.jpg \
      && test "$(head -c 2 /tmp/frame.jpg | od -An -tx1 | tr -d ' ')" = ffd8; \
    fi

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
COPY --from=ffmpeg /out/bin/ffmpeg /usr/local/bin/ffmpeg
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
