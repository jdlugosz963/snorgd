# syntax=docker/dockerfile:1

# ---- build stage --------------------------------------------------------------
# snorg and snif are public modules, so they fetch over the normal proxy — no auth needed.
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/snorgd ./cmd/snorgd

# ---- runtime stage ------------------------------------------------------------
# go-git drives git in-process, so the image needs no git binary, no openssh-client,
# and no Python — just CA certs for HTTPS remotes and the notes repo's TLS.
#
# The rest is the typesetter for letter.pdf_command, and it is almost all of the
# image's size: ~1 GB, against ~40 MB without it. It buys the collected answers PDF
# published back to the device; drop these packages and set pdf_command to "" to get
# the small image back, and the answers still land in the archive and are pushed with
# the batch — only the document published to the device is skipped.
#
# internal/letter/letters.tex targets pdflatex (fontenc/lmodern, not fontspec), so
# xetex is not needed. The three texmf-dist collections are: latexrecommended for
# booktabs, latexextra for quoting, fontsrecommended for lmodern. The package is
# pandoc-cli — Alpine has no plain "pandoc".
FROM alpine:3
RUN apk add --no-cache \
      ca-certificates \
      pandoc-cli \
      texlive \
      texmf-dist-latexrecommended \
      texmf-dist-latexextra \
      texmf-dist-fontsrecommended

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
