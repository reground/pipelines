# pipelines

A small engine for things that move through stages. A *pipeline* is a plain
text file in git that says what the stages are; people (called *contacts*)
travel through it as *items*; every move is written to an append-only log;
and the numbers you care about are *views* defined in the same file.

Built first for listswap partnerships. The engine knows nothing about
listswaps: the pipeline file defines the fields, stages, templates and
questions. This repo is the engine; the pipeline files live in a separate
repo (`reground-pipelines`), the same split as `eec` and `eec-courses`.

## Run it locally

```sh
# Postgres, any way you like. For example:
docker run -d --name pipelines-dev-pg -e POSTGRES_PASSWORD=dev \
  -e POSTGRES_DB=pipelines -p 127.0.0.1:5433:5432 postgres:16

export DATABASE_URL='postgres://postgres:dev@127.0.0.1:5433/pipelines?sslmode=disable'
export PIPELINES_DIR=../reground-pipelines     # a folder of pipeline folders
# or, to try CI sync locally: PIPELINES_DATA=/some/dir SYNC_TOKEN=secret
export LISTEN=127.0.0.1:8080                   # the default

go run . serve        # the web app (applies the schema on start)
go run . check DIR    # check pipeline files; exit 1 if there are problems
go test ./...         # set TEST_DATABASE_URL to run the database tests
```

There is no sign-in. It is meant to sit behind the hub's SSO proxy and
listens on localhost by default. Browser form posts from other sites are
refused.

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

The definitions repo's CI does two things on every push to `master`: it
runs `pipelines check .` (installing a pinned tag of this repo, the same
way the campaign repos install `eec-campaigns`), and then sends the files
to the app with `POST /admin/sync`, as `eec-courses` does for `eec`.

The endpoint takes a gzipped tar with one folder per pipeline, checks it
with the same checker, and only if it passes installs it as a new release
(`<data>/releases/<time>/`) and switches `<data>/current` to it with one
atomic rename. A set that fails the check is refused with the problems
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
