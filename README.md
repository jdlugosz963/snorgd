# snorgd

A daemon that watches Dropbox for `.note` changes, ingests each into a local
[snorg](https://github.com/jdlugosz963/snorg) archive, and commits + pushes that change to the notes repo.

## Run

Copy `snorgd.env.example` to `snorgd.env` and fill it in, then either build and run
locally, or run the container:

```sh
# local
go build -o snorgd ./cmd/snorgd && source snorgd.env && ./snorgd

# container (docker or podman) — put a push-capable SSH key + known_hosts in ./ssh first
docker compose up --build
```

Git runs in-process (go-git) and `.note` parsing is native, so nothing beyond the
binary is required at runtime — no `git`, `supernote-tool`, or Python. You just need a
Dropbox app refresh token and, when `SNORGD_REMOTE` is an SSH URL, a private key and a
matching `known_hosts` file (host keys are verified strictly). See
`snorgd.env.example` for the full list of environment variables.
