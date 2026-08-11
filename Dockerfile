# syntax=docker/dockerfile:1

# ---- build stage --------------------------------------------------------------
# snorg is a public module, so it fetches over the normal proxy — no auth needed.
FROM golang:1.24 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/snorgd ./cmd/snorgd

# ---- runtime stage ------------------------------------------------------------
# python:3.12-slim gives us Python for supernote-tool (supernotelib); git + SSH are
# needed at runtime to clone/push the archive to SNORGD_REMOTE.
FROM python:3.12-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends git openssh-client ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    pip install --no-cache-dir supernotelib

# Unprivileged user. /data holds the archive and cursor state — pre-create the mount
# points and hand them to the user so bind/volume mounts are writable (this also fixes
# fresh named-volume ownership under rootless podman on Fedora).
RUN useradd --create-home --uid 10001 app && \
    mkdir -p /data/archive /data/state && \
    chown -R app:app /data
COPY --from=build /out/snorgd /usr/local/bin/snorgd
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

USER app
ENV HOME=/home/app
WORKDIR /home/app
# Trust the git remote host on first connect (TOFU) rather than failing the push;
# known_hosts is written into the writable ~/.ssh below.
ENV GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=accept-new"

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
