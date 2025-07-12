# Build stage
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY *.go ./

# Build the application
RUN CGO_ENABLED=1 go build -o stremio-server .

# FFmpeg build stage
FROM alpine:3.18 AS ffmpeg

# Install FFmpeg build dependencies
RUN apk add --no-cache --virtual .build-dependencies \
    gnutls \
    freetype-dev \
    gnutls-dev \
    lame-dev \
    libass-dev \
    libogg-dev \
    libtheora-dev \
    libvorbis-dev \
    libvpx-dev \
    libwebp-dev \
    libssh2 \
    opus-dev \
    rtmpdump-dev \
    x264-dev \
    x265-dev \
    yasm-dev \
    build-base \
    coreutils \
    gnutls \
    nasm \
    dav1d-dev \
    libbluray-dev \
    libdrm-dev \
    zimg-dev \
    aom-dev \
    xvidcore-dev \
    fdk-aac-dev \
    libva-dev \
    git \
    x264

# Build FFmpeg from Jellyfin source
ENV BIN="/usr/bin"
RUN cd && \
    DIR=$(mktemp -d) && \
    cd "${DIR}" && \
    git clone --depth 1 --branch v4.4.1-4 https://github.com/jellyfin/jellyfin-ffmpeg.git && \
    cd jellyfin-ffmpeg* && \
    PATH="$BIN:$PATH" && \
    ./configure --help && \
    ./configure --bindir="$BIN" --disable-debug \
    --prefix=/usr/lib/jellyfin-ffmpeg --extra-version=Jellyfin --disable-doc --disable-ffplay --disable-shared --disable-libxcb --disable-sdl2 --disable-xlib --enable-lto --enable-gpl --enable-version3 --enable-gmp --enable-gnutls --enable-libdrm --enable-libass --enable-libfreetype --enable-libfribidi --enable-libfontconfig --enable-libbluray --enable-libmp3lame --enable-libopus --enable-libtheora --enable-libvorbis --enable-libdav1d --enable-libwebp --enable-libvpx --enable-libx264 --enable-libx265  --enable-libzimg --enable-small --enable-nonfree --enable-libxvid --enable-libaom --enable-libfdk_aac --enable-vaapi --enable-hwaccel=h264_vaapi --enable-hwaccel=hevc_vaapi --toolchain=hardened && \
    make -j4 && \
    make install && \
    make distclean && \
    rm -rf "${DIR}"  && \
    apk del --purge .build-dependencies

# Runtime stage
FROM alpine:latest

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Install FFmpeg runtime dependencies
RUN apk add --no-cache \
    libwebp \
    libvorbis \
    x265-libs \
    x264-libs \
    libass \
    opus \
    libgmpxx \
    lame-libs \
    gnutls \
    libvpx \
    libtheora \
    libdrm \
    libbluray \
    zimg \
    libdav1d \
    aom-libs \
    xvidcore \
    fdk-aac \
    libva \
    curl

# Add arch specific libs
RUN if [ "$(uname -m)" = "x86_64" ]; then \
    apk add --no-cache intel-media-driver mesa-va-gallium; \
    fi

# Copy FFmpeg from build stage
COPY --from=ffmpeg /usr/bin/ffmpeg /usr/bin/ffprobe /usr/bin/
COPY --from=ffmpeg /usr/lib/jellyfin-ffmpeg /usr/lib/

# Create non-root user
RUN addgroup -g 1000 stremio && \
    adduser -D -s /bin/sh -u 1000 -G stremio stremio

# Set working directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/stremio-server .

# Copy web UI files (if available)
COPY --chown=stremio:stremio build/ ./build/

# Create cache directory
RUN mkdir -p /root/.stremio-server/cache /root/.stremio-server/thumbnails /root/.stremio-server/hls && \
    chown -R stremio:stremio /root/.stremio-server

# Switch to non-root user
USER stremio

# Expose ports
EXPOSE 11470 12470

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:11470/api/status || exit 1

# Run the server
CMD ["./stremio-server"] 