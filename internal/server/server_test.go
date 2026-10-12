package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"gitea.reground.org/will/pipelines/internal/testdb"
)

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	db := testdb.New(t)
	dir := t.TempDir()
	if err := os.CopyFS(dir+"/basic", os.DirFS("../pipeline/testdata/basic")); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func client() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func post(t *testing.T, c *http.Client, u string, form url.Values, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestWholeFlow(t *testing.T) {
	ts, _ := newTestServer(t)
	c := client()

	// Partial entry: a name alone is enough.
	res := post(t, c, ts.URL+"/p/basic/items", url.Values{"name": {"Bob Example"}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: %d", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, "/p/basic/items/1") {
		t.Fatalf("redirect: %q", loc)
	}

	// The draft fills in what is known and flags what is not.
	code, body := get(t, c, ts.URL+"/p/basic/items/1")
	if code != 200 || !strings.Contains(body, "Hi Bob,") || !strings.Contains(body, "[due missing]") {
		t.Fatalf("draft not filled in as expected (%d):\n%s", code, body)
	}
	if !strings.Contains(body, "Still missing: due") {
		t.Fatal("missing-field warning not shown")
	}

	// Supplying the date completes the draft.
	post(t, c, ts.URL+"/p/basic/items/1/fields", url.Values{"f_due": {"2026-11-04"}}, nil)
	_, body = get(t, c, ts.URL+"/p/basic/items/1")
	if !strings.Contains(body, "Wednesday, November 4, 2026") {
		t.Fatalf("date not rendered:\n%s", body)
	}

	// Bad input is refused with a message, not stored.
	res = post(t, c, ts.URL+"/p/basic/items/1/fields", url.Values{"f_due": {"next tuesday"}}, nil)
	if !strings.Contains(res.Header.Get("Location"), "error=") {
		t.Fatalf("bad date accepted: %q", res.Header.Get("Location"))
	}

	// Move forward, and the move is logged and counted.
	post(t, c, ts.URL+"/p/basic/items/1/move", url.Values{"to": {"wait_a"}}, nil)
	_, body = get(t, c, ts.URL+"/p/basic/items/1")
	if !strings.Contains(body, "moved from Start to Waiting") {
		t.Fatalf("history missing:\n%s", body)
	}
	_, home := get(t, c, ts.URL+"/")
	if !strings.Contains(home, "Last 7 days: 1") {
		t.Fatalf("pitch not counted on home:\n%s", home)
	}

	// Moving to the stage it is already in is refused.
	res = post(t, c, ts.URL+"/p/basic/items/1/move", url.Values{"to": {"wait_a"}}, nil)
	if !strings.Contains(res.Header.Get("Location"), "error=") {
		t.Fatal("no-op move accepted")
	}

	// Exports.
	code, csv := get(t, c, ts.URL+"/export/basic/items.csv")
	if code != 200 || !strings.Contains(csv, "Bob Example") {
		t.Fatalf("items export: %d %q", code, csv)
	}
	code, csv = get(t, c, ts.URL+"/export/basic/events.csv")
	if code != 200 || !strings.Contains(csv, "wait_a") {
		t.Fatalf("events export: %d %q", code, csv)
	}
}

func TestCrossSitePostsAreRefused(t *testing.T) {
	ts, _ := newTestServer(t)
	c := client()
	for name, h := range map[string]map[string]string{
		"fetch metadata": {"Sec-Fetch-Site": "cross-site"},
		"foreign origin": {"Origin": "https://elsewhere.example"},
	} {
		res := post(t, c, ts.URL+"/p/basic/items", url.Values{"name": {"X"}}, h)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: got %d want 403", name, res.StatusCode)
		}
	}
	res := post(t, c, ts.URL+"/p/basic/items", url.Values{"name": {"X"}}, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("same-origin: got %d want 303", res.StatusCode)
	}
}

func TestBrokenFileKeepsLastWorkingVersion(t *testing.T) {
	ts, s := newTestServer(t)
	c := client()
	if code, _ := get(t, c, ts.URL+"/p/basic/"); code != 200 {
		t.Fatalf("baseline: %d", code)
	}
	path := s.Dir + "/basic/pipeline.yml"
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "label: Start", "labell: Start", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := get(t, c, ts.URL+"/p/basic/")
	if code != 200 {
		t.Fatalf("pipeline vanished after a typo: %d", code)
	}
	if !strings.Contains(body, "last working version") || !strings.Contains(body, `&#34;labell&#34;`) {
		t.Fatalf("problem not explained:\n%s", body)
	}
}

func TestNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	c := client()
	for _, p := range []string{"/p/nope/", "/p/basic/items/999", "/p/basic/items/abc", "/contacts/999", "/p/basic/views/nope"} {
		if code, _ := get(t, c, ts.URL+p); code != 404 {
			t.Errorf("%s: got %d want 404", p, code)
		}
	}
}

func TestDatabaseOutageExplainsItself(t *testing.T) {
	ts, s := newTestServer(t)
	c := client()
	if code, _ := get(t, c, ts.URL+"/healthz"); code != 200 {
		t.Fatalf("healthz with a database: %d", code)
	}
	s.DB.Close() // as if the database had gone away

	code, body := get(t, c, ts.URL+"/contacts")
	if code != 500 {
		t.Fatalf("want 500, got %d", code)
	}
	if !strings.Contains(body, "database cannot be reached") || !strings.Contains(body, "Nothing you entered is lost") {
		t.Fatalf("outage page does not explain itself:\n%s", body)
	}
	if !strings.Contains(body, `href="/contacts"`) {
		t.Fatal("navigation should still be present on the error page")
	}
	// Pages that need no database keep working.
	if code, _ := get(t, c, ts.URL+"/contacts/new"); code != 200 {
		t.Fatalf("contacts/new: %d", code)
	}
	if code, _ := get(t, c, ts.URL+"/healthz"); code != 503 {
		t.Fatalf("healthz should report the outage, got %d", code)
	}
}
