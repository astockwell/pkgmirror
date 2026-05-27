# syntax=docker/dockerfile:1.7

# ---------- build stage ----------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Cache go module downloads in a dedicated layer.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source.
COPY . .

# CGO is not needed (modernc.org/sqlite is pure Go). Build a static binary.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags='-s -w' -o /out/pkgmirror ./cmd/pkgmirror

# ---------- runtime stage ----------
FROM debian:bookworm-slim

# Create a non-root user with /data as its home so the auto-created data
# dir is writable.
RUN groupadd --system --gid 10001 pkgmirror \
 && useradd  --system --uid 10001 --gid pkgmirror \
             --home-dir /data --create-home --shell /usr/sbin/nologin \
             pkgmirror \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/pkgmirror /usr/local/bin/pkgmirror

USER pkgmirror
WORKDIR /data

ENV PKGMIRROR_ADDR=:8080 \
    PKGMIRROR_DATA_DIR=/data

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/pkgmirror"]
