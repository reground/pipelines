// Package dbstore owns the Postgres schema and all queries for contacts,
// items and the event log.
package dbstore

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

//go:embed schema.sql
var schemaSQL string

type DB struct {
	*sql.DB
}

func Open(dsn string) (*DB, error) {
	sqlDB, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	if _, err := sqlDB.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &DB{sqlDB}, nil
}

var ErrNotFound = errors.New("not found")

type Contact struct {
	ID        int64
	Name      string
	Email     string
	Website   string
	Notes     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Map returns the contact as template data.
func (c Contact) Map() map[string]string {
	return map[string]string{"name": c.Name, "email": c.Email, "website": c.Website, "notes": c.Notes}
}

type Item struct {
	ID             int64
	Pipeline       string
	ContactID      int64
	ContactName    string
	ContactEmail   string
	Stage          string
	StageEnteredAt time.Time
	Fields         map[string]string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ArchivedAt     sql.NullTime
}

type Event struct {
	ID        int64
	ItemID    int64
	Pipeline  string
	Kind      string
	FromStage string
	ToStage   string
	Note      string
	At        time.Time
}

// ---- contacts

const contactCols = `id, name, email, website, notes, created_at, updated_at`

func scanContact(r interface{ Scan(...any) error }) (Contact, error) {
	var c Contact
	err := r.Scan(&c.ID, &c.Name, &c.Email, &c.Website, &c.Notes, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (db *DB) CreateContact(name, email, website, notes string) (Contact, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Contact{}, errors.New("a contact needs a name")
	}
	return scanContact(db.QueryRow(
		`INSERT INTO contacts (name, email, website, notes) VALUES ($1,$2,$3,$4) RETURNING `+contactCols,
		name, strings.TrimSpace(email), strings.TrimSpace(website), notes))
}

func (db *DB) UpdateContact(id int64, name, email, website, notes string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a contact needs a name")
	}
	res, err := db.Exec(
		`UPDATE contacts SET name=$2, email=$3, website=$4, notes=$5, updated_at=now() WHERE id=$1`,
		id, name, strings.TrimSpace(email), strings.TrimSpace(website), notes)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) GetContact(id int64) (Contact, error) {
	c, err := scanContact(db.QueryRow(`SELECT `+contactCols+` FROM contacts WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	return c, err
}

// ListContacts returns contacts whose name or email contains q, by name.
func (db *DB) ListContacts(q string) ([]Contact, error) {
	rows, err := db.Query(`SELECT `+contactCols+` FROM contacts
		WHERE $1 = '' OR name ILIKE '%' || $1 || '%' OR email ILIKE '%' || $1 || '%'
		ORDER BY lower(name), id`, strings.TrimSpace(q))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- items

const itemCols = `i.id, i.pipeline, i.contact_id, c.name, c.email, i.stage, i.stage_entered_at,
	i.fields, i.created_at, i.updated_at, i.archived_at`

const itemFrom = ` FROM items i JOIN contacts c ON c.id = i.contact_id`

func scanItem(r interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var raw []byte
	err := r.Scan(&it.ID, &it.Pipeline, &it.ContactID, &it.ContactName, &it.ContactEmail,
		&it.Stage, &it.StageEnteredAt, &raw, &it.CreatedAt, &it.UpdatedAt, &it.ArchivedAt)
	if err != nil {
		return it, err
	}
	it.Fields = map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &it.Fields); err != nil {
			return it, err
		}
	}
	return it, nil
}

// CreateItem starts a contact in a pipeline at the given stage and records
// the "created" event.
func (db *DB) CreateItem(pipelineID string, contactID int64, stage string, fields map[string]string) (Item, error) {
	raw, _ := json.Marshal(cleanFields(fields))
	tx, err := db.Begin()
	if err != nil {
		return Item{}, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(
		`INSERT INTO items (pipeline, contact_id, stage, fields) VALUES ($1,$2,$3,$4) RETURNING id`,
		pipelineID, contactID, stage, raw).Scan(&id); err != nil {
		return Item{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO events (item_id, pipeline, kind, to_stage) VALUES ($1,$2,'created',$3)`,
		id, pipelineID, stage); err != nil {
		return Item{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, err
	}
	return db.GetItem(id)
}

func cleanFields(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out[k] = v
		}
	}
	return out
}

func (db *DB) GetItem(id int64) (Item, error) {
	it, err := scanItem(db.QueryRow(`SELECT `+itemCols+itemFrom+` WHERE i.id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	return it, err
}

func (db *DB) UpdateItemFields(id int64, fields map[string]string) error {
	raw, _ := json.Marshal(cleanFields(fields))
	res, err := db.Exec(`UPDATE items SET fields=$2, updated_at=now() WHERE id=$1`, id, raw)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveItem moves an item to another stage and records the move. Moving to
// the stage it is already in is an error, so the log never records a
// no-op as an event.
func (db *DB) MoveItem(id int64, to, note string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pipeline, from string
	err = tx.QueryRow(`SELECT pipeline, stage FROM items WHERE id=$1 FOR UPDATE`, id).Scan(&pipeline, &from)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if from == to {
		return fmt.Errorf("already in %q", to)
	}
	if _, err := tx.Exec(
		`UPDATE items SET stage=$2, stage_entered_at=now(), updated_at=now() WHERE id=$1`, id, to); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO events (item_id, pipeline, kind, from_stage, to_stage, note) VALUES ($1,$2,'moved',$3,$4,$5)`,
		id, pipeline, from, to, note); err != nil {
		return err
	}
	return tx.Commit()
}

// SetArchived hides an item from lists without touching its history.
func (db *DB) SetArchived(id int64, archived bool) error {
	q := `UPDATE items SET archived_at = NULL, updated_at = now() WHERE id=$1`
	if archived {
		q = `UPDATE items SET archived_at = now(), updated_at = now() WHERE id=$1`
	}
	res, err := db.Exec(q, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Filter selects items from one pipeline.
type Filter struct {
	Pipeline        string
	IDs             []int64
	ContactID       int64
	Stages          []string
	StageNot        []string
	Query           string // matches contact name or email
	DueField        string // date field id, with DueDays
	DueDays         *int
	DaysInStageOver int
	IncludeArchived bool

	// Sort is "contact", "stage", "entered", "created", or a field id.
	// SortType is that field's type (date or number sort by value).
	Sort     string
	SortType string
	Desc     bool
}

// ListItems returns items matching f. The sort key is validated by the
// caller against the pipeline; it is passed to SQL as a parameter, never
// concatenated.
func (db *DB) ListItems(f Filter) ([]Item, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where = append(where, "i.pipeline = "+arg(f.Pipeline))
	if !f.IncludeArchived {
		where = append(where, "i.archived_at IS NULL")
	}
	if len(f.IDs) > 0 {
		where = append(where, "i.id = ANY("+arg(pq.Array(f.IDs))+")")
	}
	if f.ContactID > 0 {
		where = append(where, "i.contact_id = "+arg(f.ContactID))
	}
	if len(f.Stages) > 0 {
		where = append(where, "i.stage = ANY("+arg(pq.Array(f.Stages))+")")
	}
	if len(f.StageNot) > 0 {
		where = append(where, "NOT (i.stage = ANY("+arg(pq.Array(f.StageNot))+"))")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		p := arg(q)
		where = append(where, "(c.name ILIKE '%' || "+p+" || '%' OR c.email ILIKE '%' || "+p+" || '%')")
	}
	if f.DueField != "" && f.DueDays != nil {
		fp, dp := arg(f.DueField), arg(*f.DueDays)
		where = append(where, "NULLIF(i.fields->>"+fp+", '') IS NOT NULL AND (i.fields->>"+fp+")::date <= current_date + "+dp+"::int")
	}
	if f.DaysInStageOver > 0 {
		where = append(where, "i.stage_entered_at < now() - ("+arg(f.DaysInStageOver)+"::int * interval '1 day')")
	}

	dir := "ASC"
	if f.Desc {
		dir = "DESC"
	}
	var order string
	switch f.Sort {
	case "", "contact":
		order = "lower(c.name) " + dir
	case "stage":
		order = "i.stage " + dir + ", lower(c.name)"
	case "entered":
		order = "i.stage_entered_at " + dir
	case "created":
		order = "i.created_at " + dir
	default:
		p := arg(f.Sort)
		expr := "NULLIF(i.fields->>" + p + ", '')"
		switch f.SortType {
		case "date":
			expr = "(" + expr + ")::date"
		case "number":
			expr = "(" + expr + ")::numeric"
		}
		order = expr + " " + dir + " NULLS LAST, lower(c.name)"
	}

	rows, err := db.Query(`SELECT `+itemCols+itemFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+`, i.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountsByStage counts non-archived items per stage in a pipeline.
func (db *DB) CountsByStage(pipelineID string) (map[string]int, error) {
	rows, err := db.Query(
		`SELECT stage, count(*) FROM items WHERE pipeline=$1 AND archived_at IS NULL GROUP BY stage`, pipelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// ---- events

// FirstEntries returns, for each non-archived item that ever entered one
// of stages, the time of its first such entry. Counting first entries
// means an item that bounces back and forth is one pitch, not several.
func (db *DB) FirstEntries(pipelineID string, stages []string) (map[int64]time.Time, error) {
	rows, err := db.Query(`SELECT e.item_id, min(e.at) FROM events e JOIN items i ON i.id = e.item_id
		WHERE e.pipeline=$1 AND e.to_stage = ANY($2) AND i.archived_at IS NULL
		GROUP BY e.item_id`, pipelineID, pq.Array(stages))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

// ItemEvents returns an item's history, oldest first.
func (db *DB) ItemEvents(itemID int64) ([]Event, error) {
	return db.events(`WHERE item_id=$1 ORDER BY at, id`, itemID)
}

// AllEvents returns every event in a pipeline, oldest first.
func (db *DB) AllEvents(pipelineID string) ([]Event, error) {
	return db.events(`WHERE pipeline=$1 ORDER BY at, id`, pipelineID)
}

func (db *DB) events(tail string, arg any) ([]Event, error) {
	rows, err := db.Query(
		`SELECT id, item_id, pipeline, kind, from_stage, to_stage, note, at FROM events `+tail, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.ItemID, &e.Pipeline, &e.Kind, &e.FromStage, &e.ToStage, &e.Note, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
