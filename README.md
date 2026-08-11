# snorgd

A daemon that watches Dropbox for `.note` changes, ingests each into a local
[snorg](https://github.com/jdlugosz963/snorg) archive, and commits + pushes that change to the notes repo.

## Run

Copy `snorgd.env.example` to `snorgd.env` and fill it in, then either build and run
locally, or run the container:

```sh
# local
go build -o snorgd ./cmd/snorgd && source snorgd.env && ./snorgd

# container (docker or podman) — put a push-capable SSH key in ./ssh first
docker compose up --build
```

Needs `git`, `supernote-tool`, and a Dropbox app refresh token; see
`snorgd.env.example` for the full list of environment variables.
