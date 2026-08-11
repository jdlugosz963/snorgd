# syntax=docker/dockerfile:1

# ---- build stage --------------------------------------------------------------
# snorg is a public module, so it fetches over the normal proxy — no auth needed.
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/snorgd ./cmd/snorgd

# ---- runtime stage ------------------------------------------------------------
# go-git drives git in-process, so the image needs no git binary, no openssh-client,
# and no Python — just CA certs for HTTPS remotes and the notes repo's TLS.
FROM alpine:3
RUN apk add --no-cache ca-certificates

# Unprivileged user. /data holds the archive and cursor state — pre-create the mount
# points and hand them to the user so bind/volume mounts are writable (this also fixes
# fresh named-volume ownership under rootless podman on Fedora).
RUN adduser -D -u 10001 app && \
    mkdir -p /data/archive /data/state && \
    chown -R app:app /data
COPY --from=build /out/snorgd /usr/local/bin/snorgd

USER app
ENV HOME=/home/app
WORKDIR /home/app

ENTRYPOINT ["snorgd"]
