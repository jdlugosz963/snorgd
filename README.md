# snorgd

A daemon that watches Dropbox for `.note` changes and drives them through a
configurable pipeline into a local [snorg](https://github.com/jdlugosz963/snorg)
archive, committing and pushing the whole batch as one change.

## The pipeline

A Supernote sync arrives as a burst of changed notes, so snorgd queues them rather
than reacting to each one:

1. **Ingest** — each note's Dropbox path is matched against a glob rule that decides
   whether to ingest it and which snorg tags it gets. The queue is drained and
   ingested as one batch. A tag written here is note-scoped, so every page of the
   note inherits it — which is what the analyze rules downstream select on.
2. **Quiet period** — once the queue has been empty for `quiet_period` (5s by
   default), the downstream stages fire. Anything arriving in the meantime is folded
   into the same batch and restarts the wait.
3. **Analyze** — pages selected by their snorg tags are transcribed by the vision
   model: the handwriting becomes text in a `<PAGEID>.md` sidecar, plus whatever
   custom fields the archive's `config.yaml` asks for. A page whose ink has not
   changed since its last analysis is skipped without a model call, so the stage
   costs nothing on a quiet batch.
4. **Dispatch** — every page drawn on a registered [snif](https://github.com/jdlugosz963/snif)
   template is read back as a form: its sliders, radios and written-in boxes become
   typed values. Those values are compared against what the page said last time, and a
   page whose answers have moved is handed to the handler that owns its template,
   along with the list of what changed.
5. **Commit** — one commit and push covering the whole chain, and only once nothing
   new is waiting. If a note arrives while the stages are running, the batch loops
   back to ingest instead of committing.

Stages 3 and 4 are off unless configured.

## Forms

A page drawn on a snif template is a small user interface. snorgd declares those
templates in Go, draws them with `snorgd gen-templates`, and registers a handler
against each one; nothing in the dispatcher knows what any handler does, so a new kind
of page is a new handler rather than a new stage.

One ships with it:

**Letter to AI** — a slider for how long the answer should be, a radio for the style
to answer in, and the rest of the sheet for a handwritten question. Write a question,
sync, and the model's reply is written into the page's own answer region — committed
and pushed with the batch, and readable in the notes repo beside the question that
prompted it. Optionally every answer in the archive is also typeset into one PDF,
newest page first, published back to Dropbox and so onto the device.

```sh
snorgd gen-templates              # writes snif-letter.png and snif-letter.yaml here
snorgd gen-templates -print-tex   # the LaTeX template the PDF is typeset through
```

Then put the `.yaml` in the archive and `include:` it from the archive's snorg
`config.yaml` (snorg keeps only one `templates:` list, so it replaces any that is
already there), and import the `.png` on the device as a template. snorgd writes
nothing into the archive itself — everything it does to one goes through snorg's API —
so where those files land is your decision, not its.

If the device re-encodes the PNG on import its hash no longer matches and every page
reads as untemplated; `snif bind` is the repair.

## Configuration

Two files, split by what they hold:

- **`snorgd.env`** — credentials and paths. See `snorgd.env.example`.
- **`snorgd.yaml`** — the pipeline: ingest rules, analyze rules, the dispatch stage
  and its handlers.
  Read from `<SNORGD_ARCHIVE>/snorgd.yaml` by default so the rules live with the
  notes; override with `SNORGD_CONFIG`. Optional — without it, snorgd ingests
  everything untagged and routes nothing. See `snorgd.yaml.example`.

The model provider (endpoint, model, API key) comes from the archive's own snorg
`config.yaml`, along with the `templates:` section that says which background image is
which form.

### Getting the Dropbox credentials

snorgd needs three values — `DROPBOX_APP_KEY`, `DROPBOX_APP_SECRET` and
`DROPBOX_REFRESH_TOKEN`. The first two come from the app you create; the third you
mint once by hand. A refresh token does not expire, so this is a one-time setup —
snorgd exchanges it for a short-lived access token on every run.

**1. Create the app.** At <https://www.dropbox.com/developers/apps> choose *Create
app*:

- **API**: Scoped access
- **Access type**: **Full Dropbox**. Pick this unless your Supernote syncs into the
  app's own folder — with *App folder* access, snorgd can only ever see that one
  directory, and `/Supernote` would be invisible to it.
- **Name**: anything unique.

**2. Set the permissions.** On the app's *Permissions* tab, tick:

| Scope | Why |
|---|---|
| `files.metadata.read` | list the folder and long-poll it for changes |
| `files.content.read` | download the changed `.note` files |
| `files.content.write` | upload the letters PDF — **only** needed if you set `letter.pdf_command` |

Then **Submit**. Scopes are baked into a token when it is issued, so if you change
them later you have to redo step 4 — an existing token keeps the old permissions and
fails with `missing_scope`.

**3. Copy the key and secret.** On the *Settings* tab, *App key* and *App secret* are
`DROPBOX_APP_KEY` and `DROPBOX_APP_SECRET`.

**4. Mint the refresh token.** Open this URL in a browser, with your app key
substituted in — `token_access_type=offline` is the part that makes Dropbox return a
refresh token rather than a one-off access token:

```
https://www.dropbox.com/oauth2/authorize?client_id=<APP_KEY>&response_type=code&token_access_type=offline
```

Approve the app, copy the authorization code it shows, and exchange it (the code is
single-use and expires within minutes, so do this straight away):

```sh
curl -u <APP_KEY>:<APP_SECRET> https://api.dropbox.com/oauth2/token \
  -d grant_type=authorization_code \
  -d code=<AUTHORIZATION_CODE>
```

The `refresh_token` field of the JSON response is `DROPBOX_REFRESH_TOKEN`. Ignore the
`access_token` in that response — it expires in a few hours and snorgd fetches its
own.

**5. Point snorgd at a folder.** `DROPBOX_FOLDER` is watched recursively (`/` watches
everything). It must match the case-sensitive display path, e.g. `/Supernote`.

Verify the three values before starting the daemon:

```sh
source snorgd.env
curl -sS -u "$DROPBOX_APP_KEY:$DROPBOX_APP_SECRET" https://api.dropbox.com/oauth2/token \
  -d grant_type=refresh_token -d refresh_token="$DROPBOX_REFRESH_TOKEN"
```

A JSON body with an `access_token` means all three are good. `invalid_client` means
the key or secret is wrong; `invalid_grant` means the refresh token is wrong or was
revoked.

## Run

Copy `snorgd.env.example` to `snorgd.env` and fill it in, then:

```sh
go build -o snorgd ./cmd/snorgd && source snorgd.env && ./snorgd
```

Git runs in-process (go-git), `.note` parsing is native, and the PDF font is
compiled in, so nothing beyond the binary is required at runtime — no `git`,
`supernote-tool`, Python, or system fonts. You need a Dropbox app refresh token and,
when `SNORGD_REMOTE` is an SSH URL, a private key and a matching `known_hosts` file
(host keys are verified strictly).

Because git runs in-process, **`~/.ssh/config` is not consulted**: `SNORGD_REMOTE`
must spell out the user, as in `git@git.example.com:repo`, even where a `Host` block
would otherwise supply it. Host keys are verified against `SNORGD_SSH_KNOWN_HOSTS`,
advertising only the algorithms that file already holds for the host — so a single
entry of any one type is enough, and a server offering several key types will not
trip a spurious "key mismatch".

### Building the container

snorg and snif are public modules fetched over the Go proxy, so the image builds from
this repository alone:

```sh
podman build -t snorgd .    # or: docker compose build
```

The runtime image carries pandoc and a pdflatex-capable TeX for the letters PDF, and
they are almost all of its ~1 GB — without them it is around 40 MB. Dropping those
packages from the runtime stage and setting `letter.pdf_command` to `""` gets the small
image back; answers still land in the archive and are pushed with the batch, and only
the document published to the device is lost.

`compose.yaml` declares the same thing as `additional_contexts` (Compose v2.17+).
To hand the image to another machine:

```sh
podman save --format docker-archive localhost/snorgd:latest | gzip > snorgd-image.tar.gz
gunzip -c snorgd-image.tar.gz | podman load                # on the other machine
```

## Notes on behaviour

- **Schema migration is automatic.** snorg refuses to read notes written by an older
  schema, so snorgd migrates the whole archive at startup and logs what it upgraded.
- **Deletions are never propagated.** A note deleted in Dropbox is logged and left in
  the archive; snorgd does not prune.
- **The remote wins, and snorgd syncs to it before each batch.** The archive is a
  working clone you may also write to by hand, so snorgd fetches at the start of every
  batch and builds its work on the current remote tip — which is what makes its own
  push a fast-forward. Being behind is a fast-forward; unpushed commits on an unmoved
  remote are left alone and pushed normally. Only a genuine divergence resets, and the
  discarded commits are first saved under `refs/snorgd/dropped/<timestamp>`, named in
  the log, and recoverable with `git cherry-pick`. That backup matters: a note whose
  commit is discarded is gone for good otherwise, because its `.note` is deleted after
  ingest and the Dropbox cursor has already moved past it. If you push from elsewhere
  *while* a batch is running, that batch's push is rejected and the next sync treats it
  as a divergence — rare with a human-paced second writer, but the cost of resolving
  without a merge.
- **Form state lives outside the archive**, in `SNORGD_STATE_DIR/forms`, and is never
  committed. It is one JSON file per templated page holding the answers that page last
  gave, and comparing against it is the only reason snorgd knows anything changed —
  snif itself is stateless and reads the same page the same way every time. Delete a
  page's file to have it treated as new; lose the directory and every page looks new,
  costing whatever its handler does, which for the letter form is a model call each.
- **A page is only acted on when something on it moves.** An unchanged page never
  reaches its handler, and a handler that fails leaves the state untouched, so the same
  change is offered again on the next batch rather than lost.
- **A blank question is never sent to the model.** A printed sheet reads as an empty
  question on every batch until someone writes on it; that costs nothing.
- **Answers are written through snorg, not into files.** The reply goes into the
  page's `answer` region via snorg's own edit round-trip, so it is part of the page,
  survives re-analysis, and appears in `snorg export`. The box is declared
  `analyze: false`, so the vision model never overwrites it, and snif ignores it, so it
  never feeds back into the change detection.
- **Maths and code are pandoc's problem, not snorgd's.** The model is asked for
  markdown with `$...$` maths and fenced code; pandoc converts that to LaTeX, escaping
  specials in prose while leaving formulas alone, and the embedded `letters.tex` decides
  what the result looks like. snorgd contains no LaTeX escaping of its own.
- **The PDF is rebuilt from the archive every time**, newest page first — ordered by
  page id, which encodes when the page was created on the device. There is no second
  copy of the history to keep in step, and an answer edited in the repo shows up in the
  next document.
- **It is published once per batch, not once per answer.** Because the document is
  rebuilt whole, its cost belongs to the run: a sync carrying five questions answers
  five pages and then typesets and uploads a single document. The handler marks itself
  dirty in `Handle` and the dispatcher flushes it after the batch's last page.
- **A failed PDF never costs an answer.** A missing typesetter, a compile failure or an
  unreachable Dropbox is logged and the pages stay answered; only the publishing step
  is lost, and because the flush keeps its dirty flag until an upload succeeds, the
  next batch republishes without asking the model anything again.
- **The `ai-answered` tag is a marker and an index.** It flags a page for anyone
  browsing the archive, and it is how the collected PDF finds the answered pages.
