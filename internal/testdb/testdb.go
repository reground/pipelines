// Package testdb gives tests an isolated database: a fresh Postgres schema
// per test, dropped afterwards. Tests are skipped when TEST_DATABASE_URL is
// not set, so the suite still runs without Postgres.
package testdb

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"testing"

	"gitea.reground.org/will/pipelines/internal/dbstore"
)

func New(t *testing.T) *dbstore.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schema := fmt.Sprintf("t_%d", rand.Int63())
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := dbstore.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
