# pipelines

A small engine for things that move through stages. A *pipeline* is a plain
text file in git that says what the stages are; people (called *contacts*)
travel through it as *items*; every move is written to an append-only log;
and the numbers you care about are *views* defined in the same file.

The idea is that your process is a file, and the app is a view of it. To
change how you work you edit the file, check it, and push. There is no
admin screen for building pipelines, no accounts, and no plugin system.

Built first for listswap partnerships. The engine knows nothing about
listswaps: the pipeline file defines the fields, stages, templates and
questions. A worked, generic example is in
[`examples/guest_spots`](examples/guest_spots/pipeline.yml).

## What it is for, and what it is not

It suits a person or a small team who have a handful of repeatable
processes (partnership outreach, guest-spot pitches, submissions, review
copies) and want to see at a glance what needs them now, what is waiting
on someone else, and how the numbers are trending.

It is not a general CRM. Know these before you adopt it:

- **It does not send anything.** It shows you a draft filled in for the
  contact; you copy it into your own mail client.
- **It does not read your inbox or connect to other services.**
- **It is single-tenant.** One database, one team, everyone sees everything.
- **It has no sign-in of its own.** See the next section.
- **Processes are edited as YAML.** If that sounds unpleasant, this is not
  the tool for you.

## How it is meant to be deployed

pipelines trusts whoever can reach it. There are no users, passwords or
roles inside the app. Anyone who can load a page can read every contact and
change every item.

That is deliberate: it keeps the tool small. It means you must put it
behind something that decides who gets in, and you must not expose its
port directly to the internet. Acceptable arrangements are:

- bound to `127.0.0.1` on a machine only you use (the default);
- behind a reverse proxy that requires a login (HTTP basic auth, an SSO
  proxy such as Authelia or Authentik, or similar);
- on a private network or VPN.

One more thing the app does for itself: it refuses browser form posts that
come from another site. Requests that carry no browser headers (curl, CI)
are allowed, which is why `/admin/sync` has its own bearer token.

## Setting it up

This takes about twenty minutes. Nothing here is specific to any one
hosting provider or Git host.

### What you need

- A machine that can run a Go program or a Docker container. A small
  virtual server is plenty.
- PostgreSQL 14 or newer, and a database you can create tables in.
- A place to keep your pipeline files. A git repository is the intent,
  but a plain folder works for getting started.
- Go 1.23 or newer, only if you build from source (not needed with Docker).

### 1. Get the engine

Build from source:

```sh
git clone https://gitea.reground.org/will/pipelines.git
cd pipelines
git checkout v0.3.2        # use the newest tag, not an untagged commit
go build -o pipelines .
sudo install -m 0755 pipelines /usr/local/bin/pipelines
```

Or build the container image:

```sh
docker build -t pipelines .
```

(The image runs `pipelines serve`, listens on `0.0.0.0:8080` inside the
container, and has no database or pipeline files in it.)

### 2. Make a database

Any PostgreSQL will do. If you do not have one:

```sh
docker network create pipelines-net
docker run -d --name pipelines-pg --network pipelines-net \
  --restart unless-stopped \
  -e POSTGRES_PASSWORD=choose-a-password -e POSTGRES_DB=pipelines \
  -v pipelines-pgdata:/var/lib/postgresql/data \
  -p 127.0.0.1:5432:5432 postgres:16
```

From the same machine, outside Docker, the connection string is
`postgres://postgres:choose-a-password@127.0.0.1:5432/pipelines?sslmode=disable`.
From another container on `pipelines-net`, use `pipelines-pg` instead of
`127.0.0.1`.
You do not create any tables yourself: the app creates and upgrades its own
schema every time it starts, and the statements are safe to run again.

### 3. Write your pipeline files

Start from the example rather than a blank page:

```sh
mkdir -p ~/my-pipelines
cp -r examples/guest_spots ~/my-pipelines/
```

The folder layout is one folder per pipeline, each holding a
`pipeline.yml` and an optional `templates/` folder of `.md` drafts. The
folder name should match the pipeline's `id`. The sections further down
document every key.

Check what you have:

```sh
pipelines check ~/my-pipelines
```

You want every pipeline to print `OK`. Anything wrong is reported in plain
English, for example a `next:` that names a stage that does not exist, or a
template that uses a field that is not defined. Unknown keys are an error,
so a typo is caught rather than ignored.

### 4. Run it

The app is configured entirely by environment variables:

- `DATABASE_URL` (required): the Postgres connection string.
- `PIPELINES_DIR`: a fixed folder of pipeline folders. Use this for the
  simplest setup; changing your pipelines then means editing files and
  restarting.
- `PIPELINES_DATA` and `SYNC_TOKEN`: use these instead of `PIPELINES_DIR`
  if you want to push new definitions to a running app without restarting
  it (see "Publishing definitions from CI" below). `PIPELINES_DATA` must be
  a directory the app can write to. The app manages a `current` link inside
  it, pointing at the newest release; leave that name to it.
- `LISTEN`: address and port. Defaults to `127.0.0.1:8080`.

Set `PIPELINES_DIR` or `PIPELINES_DATA`, not both.

Directly:

```sh
export DATABASE_URL='postgres://postgres:choose-a-password@127.0.0.1:5432/pipelines?sslmode=disable'
export PIPELINES_DIR=$HOME/my-pipelines
pipelines serve
```

Then open `http://127.0.0.1:8080/`. You should see the Home page with the
example's questions on it. `GET /healthz` answers `ok` when the app can
reach the database, which is what a monitor should poll.

With Docker, with the pipelines mounted read-only:

```sh
docker run -d --name pipelines --restart unless-stopped \
  --network pipelines-net \
  -p 127.0.0.1:8080:8080 \
  -e DATABASE_URL='postgres://postgres:choose-a-password@pipelines-pg:5432/pipelines?sslmode=disable' \
  -e PIPELINES_DIR=/pipelines \
  -v "$HOME/my-pipelines:/pipelines:ro" \
  pipelines
```

Note the `127.0.0.1:` in `-p`. Without it Docker publishes the port on all
interfaces, which bypasses the rule that nothing reaches the app except
through your login.

As a systemd service, an example unit
(`/etc/systemd/system/pipelines.service`):

```ini
[Unit]
Description=pipelines
After=network.target postgresql.service

[Service]
User=pipelines
Environment=DATABASE_URL=postgres://pipelines:PASSWORD@127.0.0.1:5432/pipelines?sslmode=disable
Environment=PIPELINES_DIR=/srv/pipelines/definitions
Environment=LISTEN=127.0.0.1:8080
ExecStart=/usr/local/bin/pipelines serve
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

If a pipeline file is broken when the app starts, it still starts: broken
pipelines are hidden, the problems are shown at the top of every page, and
the log says why.

### 5. Put a login in front of it

Do this before you point a real domain at it. With Caddy and HTTP basic
auth, the whole configuration is:

```
pipelines.example.org {
	basic_auth {
		# make the hash with: caddy hash-password
		yourname $2a$14$...
	}
	reverse_proxy 127.0.0.1:8080
}
```

With an SSO proxy, use that proxy's forward-auth directive instead and
keep `/healthz` open if you want an external monitor to reach it.

Two things that trip people up:

- The proxy must pass the original `Host` header through (Caddy and nginx's
  `proxy_set_header Host $host` do). The app compares a form post's
  `Origin` to the `Host` it sees, so a rewritten `Host` makes every button
  answer "cross-site request refused".
- `/admin/sync` is protected by its own bearer token, not by your login.
  If your login would block CI, either exempt that one path in the proxy
  or send CI through an internal address that skips the proxy.

### 6. Add contacts and entries

On the Contacts page, add a person (only a name is required). Open a
pipeline from Home and use "new" to add an entry for that contact. Each
entry's page shows what to do at its stage, drafts filled in for the
contact, and buttons to move it forward.

### 7. Publishing definitions from CI (optional)

With `PIPELINES_DIR` you change a process by editing files on the server.
If you would rather keep the files in a git repository and have every push
update the running app, use `PIPELINES_DATA` and `SYNC_TOKEN` instead.

1. Generate a long random token (`openssl rand -hex 32`) and give it to the
   app as `SYNC_TOKEN`. Create the data directory and make it writable by
   the user the app runs as.
2. Start the app. Until the first sync it shows no pipelines.
3. From your CI (or by hand), check the files, then send them:

```sh
pipelines check .
tar czf /tmp/definitions.tar.gz folder1 folder2        # the pipeline folders
curl -s -X POST https://pipelines.example.org/admin/sync \
  -H "Authorization: Bearer $SYNC_TOKEN" \
  --data-binary @/tmp/definitions.tar.gz
```

A good answer is `{"ok":true, ...}` with the release name. A set that
fails the checker is refused with the problems listed and the live one
stays untouched. A wrong token is a 401. If `SYNC_TOKEN` or `PIPELINES_DATA`
was not set, the endpoint answers 503.

In CI, install the engine at a pinned tag, run `pipelines check .` so a
mistake fails the run, then on your main branch only, run the `tar` and
`curl` above with the token stored as a CI secret. Pushes to other branches
should be checked but never sent.

### 8. Backups

Everything the app knows lives in the one Postgres database, and the
pipeline files are in git. So the only thing to back up is the database:

```sh
pg_dump --format=custom "$DATABASE_URL" > pipelines-$(date +%F).dump
```

Restore into an empty database with `pg_restore --dbname "$DATABASE_URL"
FILE`. Run it from a timer and keep copies off the machine.

The app's own CSV exports (items, contacts, and the event log, linked from
the pages) are a convenient second copy and a way to leave.

### 9. Upgrading

Check out the newer tag, rebuild, and restart. The schema is brought up to
date when the app starts. Read the tag's changes first: if a new engine
version adds file-format keys, a definitions set that uses them will be
refused by an older engine. Upgrade the engine before pushing definitions
that need it, and bump the pinned version in your CI at the same time.

### If something goes wrong

- **"PIPELINES_DIR (or PIPELINES_DATA) is required"**: set one of them.
- **"DATABASE_URL is required"**, or a database error on start: check the
  connection string, and that Postgres accepts connections from where the
  app runs.
- **A banner of problems on every page**: the live files fail the checker.
  Run `pipelines check` on the same files and fix what it says.
- **"no pipelines found"**: each pipeline must be a folder that directly
  contains `pipeline.yml`. If you pointed at the folder that holds the
  pipeline folder's contents, go up one level.
- **"cross-site request refused"** on every button: the proxy is changing
  the `Host` header (see step 5).
- **Sync answers 500 "could not install"**: most likely the data directory
  is not writable by the user the app runs as. The server log has the exact
  message.

## Trying it without a server

To look around on your own machine and throw it away afterwards:

```sh
docker run -d --rm --name pipelines-dev-pg -e POSTGRES_PASSWORD=dev \
  -e POSTGRES_DB=pipelines -p 127.0.0.1:5433:5432 postgres:16

export DATABASE_URL='postgres://postgres:dev@127.0.0.1:5433/pipelines?sslmode=disable'
export PIPELINES_DIR=./examples     # the folder of pipeline folders
go run . serve                      # then open http://127.0.0.1:8080/
go run . check examples             # check pipeline files; exit 1 if problems
go test ./...                       # set TEST_DATABASE_URL to run the database tests
docker stop pipelines-dev-pg        # when finished; --rm removes it
```

## What the pages are

- **Home**: every question (view) of every pipeline, stacked. Every number
  is a link to the exact entries behind it.
- **A pipeline's list**: sortable by column, filterable by name and stage.
- **An entry**: what to do at this stage, any drafts filled in for this
  contact, buttons to move it forward, its details, and its history.
- **Contacts**: people, with only a name required. The list shows which
  pipelines each person is in (each entry links straight to it) and can be
  filtered to people in a given pipeline, in any, or in none.
- **How this pipeline works**: the pipeline file, read back in plain text.
- CSV exports of items, contacts and the event log.

No JavaScript. Every control is a link or a form with a visible label.

## Data model

- **contacts**: a person. Name required; email, website, notes optional.
- **items**: one contact's trip through one pipeline. Carries the
  pipeline's own fields. A contact can have many items over time.
  Archiving hides an item from lists and counts without deleting anything.
- **events**: every creation and move, with the date. Append-only,
  enforced by a database trigger. All counts and rates derive from it.
- **pipelines**: not in the database. They are files.

## The pipeline file

One folder per pipeline: `<name>/pipeline.yml` and `<name>/templates/*.md`.
Unknown keys are an error, so a typo is caught instead of ignored.

```yaml
id: listswap                 # lowercase letters, digits, underscores
label: Listswap partnerships

fields:                      # what each item carries
  - {id: swap_date, label: Swap date, type: date}   # text longtext number date url

stages:                      # in order; the first is where new items start
  - id: prospect
    label: Prospect
    type: draft              # do | draft | send | wait
    help: Text shown at this stage.
    templates: [pitch-email.md]   # shown filled in for the contact
    checklist: [Something to remember]
    next: pitched            # the default step forward
    exits: [declined]        # other places it can go
    repeat:                  # optional: something you can do again here
      id: reminder           # unique in the pipeline; views refer to it
      label: Sent a reminder
      templates: [pitch-reminder.md]   # optional drafts for this action
  - id: pitched
    label: Pitched
    type: wait               # wait means the ball is with them
    next: ready
  - id: ready
    label: Ready
    type: do
    waiting_on: none         # me | them | none overrides the type's default
    next: done
  - id: done
    label: Done
    type: do
    terminal: true           # finished; no next or exits
  - id: declined
    label: Declined
    type: do
    terminal: true
```

"Waiting on" is not a field anyone maintains. It follows from the stage:
`wait` stages are waiting on them, terminal stages on nobody, the rest on
you, unless a stage says `waiting_on:`.

### Repeating a step

Some things happen more than once at the same stage: a reminder to a
partner who has not answered. A stage can offer one `repeat:` action. On
the entry page it appears as a button (with its drafts). Pressing it:

- writes a `repeated` event to the log, with the date and an optional note;
- keeps the item where it is, and restarts "in this stage for", so
  `days_in_stage_over` measures time since the last nudge;
- shows "Done N times so far" on the entry, with each occurrence in its
  history.

A repeat is not an entry into the stage, so Pitches sent does not move.
To count repeats, use `event: {repeated: <repeat id>}` in an `events` view
(below). Terminal stages cannot have one, and a cohort cannot be built
from one.

### Templates

Plain text. Available: `{{ .Contact.name }}`, `.email`, `.website`,
`.notes`, and `{{ .Item.<field id> }}`. Helpers: `{{ first .Contact.name }}`
and `{{ date .Item.swap_date }}` ("Saturday, October 24, 2026"). Anything
empty appears as `[field missing]` and is listed above the draft, so a
half-known contact still gets a useful draft.

### Views

```yaml
views:
  - id: needs_me
    label: Needs me now
    kind: list                         # the entries matching a filter
    where:
      waiting_on: me                   # me | them
      active: true                     # not in a terminal stage
      stage: [prospect, pitched]       # or stage_not: [done]
      due_within: {field: swap_date, days: 14}   # on or before today + 14, overdue included
      days_in_stage_over: 10
    sort: swap_date                    # contact | stage | entered | created | a field id
    desc: false

  - {id: by_stage, label: Partnerships by stage, kind: counts}

  - id: pitches
    label: Pitches sent
    kind: events                       # first entry into a stage, per window
    event: {entered: pitched}
    windows: [7, 30, 90]               # shows count, per day, and the period before

  - id: reply_rate
    label: Reply rate
    kind: cohort                       # of those who entered X, how many reached Y
    event: {entered: pitched}
    outcome: {entered: [ready, declined]}
    window_days: 30
    ending_days_ago: 14                # gives recent pitches time to answer
```

```yaml
  - id: reminders
    label: Reminders sent
    kind: events
    event: {repeated: reminder}        # every time, not first time
    windows: [7, 30, 90]
```

Events with `entered:` count each item's *first* entry into the stage, so
an item that bounces back counts once. With `repeated:` every occurrence
counts, and the note under each number says how many entries they were
spread across. They measure what happened, not where things
sit now: an item that was pitched last week and has since moved on is
still a pitch.

## Changing a pipeline

Edit the file, run `pipelines check`, push.

- Renaming a stage's `label` is free. Its `id` is what the log stores.
- Adding a stage is free.
- Removing a stage that still has items in it is allowed: those items
  show up on Home under "no longer in the pipeline" and each can be moved
  to a current stage. The checker will also tell you about any stage,
  exit or view that still points at the removed one.
- Changing history is not possible; the log is append-only.

## How definitions reach a running app

The definitions repo's CI does two things on every push to the main
branch: it runs `pipelines check .` (installing a pinned tag of this repo),
and then sends the files to the app with `POST /admin/sync`. Step 7 of
"Setting it up" shows the commands.

The endpoint takes a gzipped tar with one folder per pipeline, checks it
with the same checker, and only if it passes installs it as a new release
(`<data>/releases/<time>/`) and switches `<data>/current` to it with one
atomic rename. If something that is not a link is already at
`<data>/current` (an empty folder made while setting up, or a hand-placed
set of pipelines), an empty folder is replaced and anything else is moved
to `<data>/current.before-sync-<time>` so nothing is lost. A set that fails the check is refused with the problems
listed and nothing the app is serving changes, whoever sent it. Archives
may contain only plain files and folders; links, hidden paths and
oversized files are refused. The newest five releases are kept.

Enable it by setting `PIPELINES_DATA` (a writable directory) and
`SYNC_TOKEN`; the app then reads `PIPELINES_DATA/current`. Without them,
`PIPELINES_DIR` is read as a fixed directory and `/admin/sync` answers
503. Bump the pinned tag in the definitions workflow when the engine gains
a feature the definitions start to use.

If a file in the live directory is broken anyway (edited by hand, say),
the app keeps serving the last working version and shows the problems at
the top of every page.

## Layout

```
main.go                       serve and check commands
internal/pipeline/            file format, loading, the checker, templates
internal/defs/                receiving, checking and installing synced definitions
internal/dbstore/             Postgres schema and queries
internal/metrics/             evaluating views and the entries behind them
internal/server/              pages, templates, static CSS
internal/testdb/              throwaway schema per test
```

## Licence

pipelines is free software under the GNU Affero General Public License,
version 3 or (at your option) any later version. The full text is in
[`LICENSE`](LICENSE).

In practice: you can run it for yourself or your team, and change it, with
no obligation beyond keeping the licence notices. If you offer a modified
version to other people as a service over a network, you must make your
modified source available to those people. The pipeline files you write
for it are your own data and are not covered.
