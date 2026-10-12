CREATE TABLE IF NOT EXISTS contacts (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    email      TEXT NOT NULL DEFAULT '',
    website    TEXT NOT NULL DEFAULT '',
    notes      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One contact's trip through one pipeline. fields holds the pipeline's own
-- fields by id, as text; the pipeline file says what type each one is.
CREATE TABLE IF NOT EXISTS items (
    id               BIGSERIAL PRIMARY KEY,
    pipeline         TEXT NOT NULL,
    contact_id       BIGINT NOT NULL REFERENCES contacts(id),
    stage            TEXT NOT NULL,
    stage_entered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    fields           JSONB NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS items_pipeline_stage ON items (pipeline, stage);

-- Every move, in order. Append-only: the trigger below refuses to change
-- or remove a row, so the numbers derived from it stay trustworthy.
CREATE TABLE IF NOT EXISTS events (
    id         BIGSERIAL PRIMARY KEY,
    item_id    BIGINT NOT NULL REFERENCES items(id),
    pipeline   TEXT NOT NULL,
    kind       TEXT NOT NULL,         -- created | moved | repeated
    from_stage TEXT NOT NULL DEFAULT '',
    to_stage   TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Which repeat action a "repeated" event was (empty for other kinds).
ALTER TABLE events ADD COLUMN IF NOT EXISTS repeat_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS events_pipeline_stage_at ON events (pipeline, to_stage, at);
CREATE INDEX IF NOT EXISTS events_item ON events (item_id, at);

CREATE OR REPLACE FUNCTION events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'the events table is append-only';
END;
$$ LANGUAGE plpgsql;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'events_no_change' AND tgrelid = 'events'::regclass) THEN
        CREATE TRIGGER events_no_change BEFORE UPDATE OR DELETE ON events
            FOR EACH ROW EXECUTE FUNCTION events_append_only();
    END IF;
END $$;
