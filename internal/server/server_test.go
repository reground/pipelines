package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
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

// ---- definitions sync

func syncTgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

const syncGood = `id: basic
label: Basic Synced
fields: []
stages:
  - {id: start, label: Start, type: do, next: end}
  - {id: end, label: End, type: do, terminal: true}
`

func newSyncServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	db := testdb.New(t)
	data := t.TempDir()
	s, err := New(db, data+"/current")
	if err != nil {
		t.Fatal(err)
	}
	s.DataDir, s.SyncToken = data, "s3cret"
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, data
}

func doSync(t *testing.T, base, token string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/admin/sync", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestSyncRequiresTheToken(t *testing.T) {
	ts, _ := newSyncServer(t)
	body := syncTgz(t, map[string]string{"basic/pipeline.yml": syncGood})
	if code, _ := doSync(t, ts.URL, "", body); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := doSync(t, ts.URL, "wrong", body); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
}

func TestSyncDisabledWhenNotConfigured(t *testing.T) {
	ts, _ := newTestServer(t) // no SyncToken / DataDir
	if code, _ := doSync(t, ts.URL, "anything", nil); code != 503 {
		t.Fatalf("got %d, want 503", code)
	}
}

func TestSyncInstallsAndRefusesBrokenSets(t *testing.T) {
	ts, _ := newSyncServer(t)
	c := client()

	// Before the first sync there is nothing to show, and it says so.
	_, body := get(t, c, ts.URL+"/")
	if !strings.Contains(body, "No pipeline definitions are installed yet") {
		t.Fatalf("first-run message missing:\n%s", body)
	}

	good := syncTgz(t, map[string]string{"basic/pipeline.yml": syncGood})
	code, out := doSync(t, ts.URL, "s3cret", good)
	if code != 200 || out["ok"] != true || out["release"] == "" {
		t.Fatalf("good sync: %d %v", code, out)
	}
	if _, body := get(t, c, ts.URL+"/"); !strings.Contains(body, "Basic Synced") {
		t.Fatalf("new definitions not visible:\n%s", body)
	}

	// A broken set is refused with the problem named, and the live set stays.
	bad := syncTgz(t, map[string]string{"basic/pipeline.yml": strings.Replace(syncGood, "next: end", "next: nope", 1)})
	code, out = doSync(t, ts.URL, "s3cret", bad)
	if code != 422 || out["ok"] != false {
		t.Fatalf("broken sync: %d %v", code, out)
	}
	if !strings.Contains(fmt.Sprint(out["problems"]), `"nope"`) {
		t.Fatalf("problems should name the mistake: %v", out["problems"])
	}
	if code, body := get(t, c, ts.URL+"/p/basic/"); code != 200 || !strings.Contains(body, "Basic Synced") {
		t.Fatalf("live definitions changed after a refused sync (%d)", code)
	}

	// Not an archive at all.
	if code, _ := doSync(t, ts.URL, "s3cret", []byte("garbage")); code != 400 {
		t.Errorf("garbage upload: %d", code)
	}
}

func TestSyncWarnsAboutItemsInARemovedStage(t *testing.T) {
	ts, _ := newSyncServer(t)
	c := client()
	doSync(t, ts.URL, "s3cret", syncTgz(t, map[string]string{"basic/pipeline.yml": syncGood}))
	post(t, c, ts.URL+"/p/basic/items", url.Values{"name": {"Bob"}}, nil) // lands in "start"

	renamed := strings.NewReplacer("id: start", "id: begin", "next: end", "next: end").Replace(syncGood)
	code, out := doSync(t, ts.URL, "s3cret", syncTgz(t, map[string]string{"basic/pipeline.yml": renamed}))
	if code != 200 {
		t.Fatalf("sync: %d %v", code, out)
	}
	if !strings.Contains(fmt.Sprint(out["warnings"]), `"start"`) {
		t.Fatalf("expected a warning about the removed stage, got %v", out["warnings"])
	}
}

// ---- where am I, and contacts across pipelines

func h1(body string) string {
	i := strings.Index(body, "<h1>")
	j := strings.Index(body, "</h1>")
	if i < 0 || j < i {
		return ""
	}
	return body[i+4 : j]
}

func TestPagesSayWhereYouAre(t *testing.T) {
	ts, _ := newTestServer(t)
	c := client()
	post(t, c, ts.URL+"/p/basic/items", url.Values{"name": {"Bob Example"}}, nil)

	cases := []struct {
		path, h1, crumb string
	}{
		{"/p/basic/views/open", "Basic: Open", `<span aria-current="page">Open</span>`},
		{"/p/basic/items/1", "Basic: Bob Example", `<span aria-current="page">Bob Example</span>`},
		{"/p/basic/", "Basic", `<span aria-current="page">Basic</span>`},
		{"/p/basic/definition", "Basic: how this pipeline works", `<span aria-current="page">How this pipeline works</span>`},
		{"/contacts", "Contacts", `<span aria-current="page">Contacts</span>`},
	}
	for _, tc := range cases {
		code, body := get(t, c, ts.URL+tc.path)
		if code != 200 {
			t.Errorf("%s: %d", tc.path, code)
			continue
		}
		if got := h1(body); got != tc.h1 {
			t.Errorf("%s: h1 = %q, want %q", tc.path, got, tc.h1)
		}
		if !strings.Contains(body, `aria-label="Breadcrumb"`) || !strings.Contains(body, tc.crumb) {
			t.Errorf("%s: breadcrumb missing %q", tc.path, tc.crumb)
		}
		if strings.Count(body, "<h1>") != 1 {
			t.Errorf("%s: want exactly one h1", tc.path)
		}
	}

	// A drill-down page names the pipeline in its document title too.
	_, body := get(t, c, ts.URL+"/p/basic/views/open")
	if !strings.Contains(body, "<title>Basic: Open - Pipelines</title>") {
		t.Errorf("document title does not carry the pipeline name")
	}
	// The list says what it is showing.
	_, body = get(t, c, ts.URL+"/p/basic/?show=all")
	if !strings.Contains(body, "Showing: Everything") {
		t.Errorf("list does not say what it is showing")
	}
}

const secondPipeline = `id: other
label: Other Process
stages:
  - {id: first, label: First, type: do, next: last}
  - {id: last, label: Last, type: do, terminal: true}
`

func TestOneContactCanBeInSeveralPipelinesAndTwiceInOne(t *testing.T) {
	ts, s := newTestServer(t)
	c := client()
	if err := os.MkdirAll(s.Dir+"/other", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Dir+"/other/pipeline.yml", []byte(secondPipeline), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bob starts in one pipeline...
	post(t, c, ts.URL+"/p/basic/items", url.Values{"name": {"Bob Example"}, "email": {"bob@example.com"}}, nil)
	// ...and the same Bob is chosen, not re-entered, for another.
	_, form := get(t, c, ts.URL+"/p/other/new")
	if !strings.Contains(form, "Bob Example (bob@example.com)") {
		t.Fatalf("the new-entry form does not offer the existing contact:\n%s", form)
	}
	res := post(t, c, ts.URL+"/p/other/items", url.Values{"contact_id": {"1"}}, nil)
	if !strings.HasPrefix(res.Header.Get("Location"), "/p/other/items/") {
		t.Fatalf("second pipeline entry not created: %q", res.Header.Get("Location"))
	}
	// And a second entry in the first pipeline: a repeat swap with the same person.
	post(t, c, ts.URL+"/p/basic/items", url.Values{"contact_id": {"1"}}, nil)

	// Still one contact.
	_, list := get(t, c, ts.URL+"/contacts")
	if strings.Count(list, "Bob Example") != 1 {
		t.Fatalf("expected a single Bob in the contacts list:\n%s", list)
	}
	// His page shows every pipeline he is in, and both entries in the first.
	_, page := get(t, c, ts.URL+"/contacts/1")
	for _, want := range []string{"Basic", "Other Process"} {
		if !strings.Contains(page, want) {
			t.Errorf("contact page does not mention %q", want)
		}
	}
	if n := strings.Count(page, "/p/basic/items/"); n != 2 {
		t.Errorf("want 2 entries linked in Basic, got %d", n)
	}
	if n := strings.Count(page, "/p/other/items/"); n != 1 {
		t.Errorf("want 1 entry linked in Other Process, got %d", n)
	}
	// The pipelines' lists are independent.
	_, other := get(t, c, ts.URL+"/p/other/")
	if strings.Count(other, "Bob Example") != 1 {
		t.Errorf("Other Process list should show Bob once")
	}
}

func TestContactsListLeadsToPipelines(t *testing.T) {
	ts, s := newTestServer(t)
	c := client()
	if err := os.MkdirAll(s.Dir+"/other", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Dir+"/other/pipeline.yml", []byte(secondPipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	post(t, c, ts.URL+"/p/basic/items", url.Values{"name": {"Bob Example"}}, nil) // contact 1
	post(t, c, ts.URL+"/p/other/items", url.Values{"contact_id": {"1"}}, nil)     // Bob in both
	post(t, c, ts.URL+"/contacts", url.Values{"name": {"Loose Contact"}}, nil)    // contact 2, nowhere
	post(t, c, ts.URL+"/p/other/items", url.Values{"name": {"Other Only"}}, nil)  // contact 3

	_, body := get(t, c, ts.URL+"/contacts")
	for _, want := range []string{
		`>Basic: Start</a>`, `>Other Process: First</a>`, // Bob's two entries, linked
		`href="/p/basic/items/1"`, `href="/p/other/items/2"`, // item ids are global across pipelines
		"None yet", // the loose contact
	} {
		if !strings.Contains(body, want) {
			t.Errorf("contacts list missing %q", want)
		}
	}

	names := func(show string) string {
		_, b := get(t, c, ts.URL+"/contacts?show="+show)
		var out []string
		for _, n := range []string{"Bob Example", "Loose Contact", "Other Only"} {
			if strings.Contains(b, ">"+n+"</a>") {
				out = append(out, n)
			}
		}
		return strings.Join(out, ",")
	}
	for show, want := range map[string]string{
		"":      "Bob Example,Loose Contact,Other Only",
		"any":   "Bob Example,Other Only",
		"none":  "Loose Contact",
		"basic": "Bob Example",
		"other": "Bob Example,Other Only",
	} {
		if got := names(show); got != want {
			t.Errorf("show=%q: got %q, want %q", show, got, want)
		}
	}
}
