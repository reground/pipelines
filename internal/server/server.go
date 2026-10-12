// Package server is the pipelines engine's HTTP surface: plain
// server-rendered pages for contacts, items, views and exports. There is no
// JavaScript; every control is a link or a form.
//
// There is no sign-in here. It is meant to sit behind the hub's SSO proxy,
// and listens on localhost by default.
package server

import (
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.reground.org/will/pipelines/internal/dbstore"
	"gitea.reground.org/will/pipelines/internal/defs"
	"gitea.reground.org/will/pipelines/internal/metrics"
	"gitea.reground.org/will/pipelines/internal/pipeline"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	DB *dbstore.DB
	// Dir holds one subdirectory per pipeline. It is re-read on every
	// request, so editing a pipeline file takes effect immediately and a
	// broken file shows its problems on the page instead of a blank error.
	Dir string
	Now func() time.Time

	// SyncToken and DataDir enable POST /admin/sync, which installs a new
	// set of definitions into DataDir (see package defs). Dir is normally
	// DataDir/current. With either unset the endpoint answers 503 and the
	// definitions are whatever is already in Dir.
	SyncToken string
	DataDir   string

	pages map[string]*template.Template

	mu   sync.Mutex
	good map[string]*pipeline.Pipeline // last version of each directory that passed its check
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// New parses the page templates and returns a ready Server.
func New(db *dbstore.DB, dir string) (*Server, error) {
	s := &Server{DB: db, Dir: dir, pages: map[string]*template.Template{}, good: map[string]*pipeline.Pipeline{}}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if base == "layout" || strings.HasPrefix(base, "_") {
			continue
		}
		t, err := template.New("").Funcs(s.funcs()).ParseFS(assets, "templates/layout.html", "templates/_*.html", n)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		s.pages[base] = t
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.Ping(); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /p/{pid}/{$}", s.handleItems)
	mux.HandleFunc("GET /p/{pid}/definition", s.handleDefinition)
	mux.HandleFunc("GET /p/{pid}/new", s.handleNewItem)
	mux.HandleFunc("POST /p/{pid}/items", s.handleCreateItem)
	mux.HandleFunc("GET /p/{pid}/items/{id}", s.handleItem)
	mux.HandleFunc("POST /p/{pid}/items/{id}/fields", s.handleItemFields)
	mux.HandleFunc("POST /p/{pid}/items/{id}/move", s.handleItemMove)
	mux.HandleFunc("POST /p/{pid}/items/{id}/archive", s.handleItemArchive)
	mux.HandleFunc("GET /p/{pid}/views/{vid}", s.handleView)

	mux.HandleFunc("POST /admin/sync", s.handleAdminSync)

	mux.HandleFunc("GET /contacts", s.handleContacts)
	mux.HandleFunc("GET /contacts/new", s.handleNewContact)
	mux.HandleFunc("POST /contacts", s.handleCreateContact)
	mux.HandleFunc("GET /contacts/{id}", s.handleContact)
	mux.HandleFunc("POST /contacts/{id}", s.handleUpdateContact)

	mux.HandleFunc("GET /export/contacts.csv", s.handleExportContacts)
	mux.HandleFunc("GET /export/{pid}/items.csv", s.handleExportItems)
	mux.HandleFunc("GET /export/{pid}/events.csv", s.handleExportEvents)
	return sameSiteOnly(mux)
}

// sameSiteOnly refuses browser form posts that come from another site.
// Requests with no browser headers at all (curl, scripts) pass.
func sameSiteOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-site request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- shared helpers

type nav struct {
	Pipelines []*pipeline.Pipeline
}

type pageData struct {
	Title    string
	Nav      nav
	Notice   string
	Error    string
	Problems []string // pipeline file problems, shown instead of the page
	Data     any
}

// loadPipelines re-reads the pipeline files. A pipeline that fails its
// check is replaced by its last working version if there is one, with the
// problems reported alongside, so a typo never takes the app away.
func (s *Server) loadPipelines() (ok []*pipeline.Pipeline, problems []string) {
	loaded, errs, err := pipeline.LoadDirs(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, []string{"No pipeline definitions are installed yet. They arrive when the definitions repo is pushed."}
	}
	if err != nil {
		return nil, []string{err.Error()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names := map[string]bool{}
	for n := range loaded {
		names[n] = true
	}
	for n := range errs {
		names[n] = true
	}
	var dirs []string
	for n := range names {
		dirs = append(dirs, n)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		var bad []string
		if err, failed := errs[dir]; failed {
			bad = append(bad, fmt.Sprintf("%s: %v", dir, err))
		} else {
			p := loaded[dir]
			pr, _ := p.Check()
			if len(pr) == 0 {
				s.good[dir] = p
				ok = append(ok, p)
				continue
			}
			for _, x := range pr {
				bad = append(bad, dir+": "+x)
			}
		}
		problems = append(problems, bad...)
		if prev, have := s.good[dir]; have {
			problems = append(problems, dir+": showing the last working version until this is fixed.")
			ok = append(ok, prev)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].ID < ok[j].ID })
	return ok, problems
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title string, data any) {
	pls, problems := s.loadPipelines()
	pd := pageData{
		Title:    title,
		Nav:      nav{Pipelines: pls},
		Notice:   r.URL.Query().Get("notice"),
		Error:    r.URL.Query().Get("error"),
		Problems: problems,
		Data:     data,
	}
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "no such page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", pd); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

// pipelineFor finds the pipeline named in the path, or writes a 404.
func (s *Server) pipelineFor(w http.ResponseWriter, r *http.Request) (*pipeline.Pipeline, bool) {
	pls, _ := s.loadPipelines()
	id := r.PathValue("pid")
	for _, p := range pls {
		if p.ID == id {
			return p, true
		}
	}
	http.NotFound(w, r)
	return nil, false
}

// serverError logs the real error and shows a page that says what kind of
// problem it is, in the normal layout so the navigation still works.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("error: %s %s: %v", r.Method, r.URL.Path, err)
	dbDown := s.DB.Ping() != nil
	w.WriteHeader(http.StatusInternalServerError)
	s.render(w, r, "error", "Something went wrong", dbDown)
}

func idParam(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil && n > 0
}

func redirectNotice(w http.ResponseWriter, r *http.Request, path, notice string) {
	http.Redirect(w, r, path+"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

func redirectError(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"niceDate": func(v string) string {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				return v
			}
			return t.Format("Jan 2, 2006")
		},
		"niceTime": func(t time.Time) string { return t.Local().Format("Jan 2, 2006 3:04 PM") },
		"daysSince": func(t time.Time) string {
			d := int(s.now().Sub(t).Hours() / 24)
			if d == 1 {
				return "1 day"
			}
			return fmt.Sprintf("%d days", d)
		},
		"waitingLabel": func(who string) string {
			switch who {
			case "me":
				return "Me"
			case "them":
				return "Them"
			}
			return "Nobody"
		},
		"waitingFor": func(p *pipeline.Pipeline, id string) string {
			st, ok := p.Stage(id)
			if !ok {
				return "Unknown"
			}
			switch st.WaitingOn() {
			case "me":
				return "Me"
			case "them":
				return "Them"
			}
			return "Nobody"
		},
		"stageLabel": func(p *pipeline.Pipeline, id string) string {
			if st, ok := p.Stage(id); ok {
				return st.Label
			}
			return id + " (no longer in the pipeline)"
		},
		"itemPath": func(p *pipeline.Pipeline, it dbstore.Item) string {
			return fmt.Sprintf("/p/%s/items/%d", p.ID, it.ID)
		},
		"isLongText": func(t string) bool { return t == pipeline.FieldLongText },
		"inputType": func(t string) string {
			switch t {
			case pipeline.FieldDate:
				return "date"
			case pipeline.FieldNumber:
				return "number"
			case pipeline.FieldURL:
				return "url"
			}
			return "text"
		},
		"field": func(it dbstore.Item, id string) string { return it.Fields[id] },
	}
}

// ---- CSV exports

func writeCSV(w http.ResponseWriter, name string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	cw := csv.NewWriter(w)
	_ = cw.WriteAll(rows)
}

func (s *Server) handleExportContacts(w http.ResponseWriter, r *http.Request) {
	cs, err := s.DB.ListContacts("")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := [][]string{{"id", "name", "email", "website", "notes", "created_at"}}
	for _, c := range cs {
		rows = append(rows, []string{strconv.FormatInt(c.ID, 10), c.Name, c.Email, c.Website, c.Notes, c.CreatedAt.Format(time.RFC3339)})
	}
	writeCSV(w, "contacts.csv", rows)
}

func (s *Server) handleExportItems(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	items, err := s.DB.ListItems(dbstore.Filter{Pipeline: p.ID, IncludeArchived: true})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	head := []string{"id", "contact", "email", "stage", "stage_entered_at", "created_at", "archived"}
	for _, f := range p.Fields {
		head = append(head, f.ID)
	}
	rows := [][]string{head}
	for _, it := range items {
		row := []string{strconv.FormatInt(it.ID, 10), it.ContactName, it.ContactEmail, it.Stage,
			it.StageEnteredAt.Format(time.RFC3339), it.CreatedAt.Format(time.RFC3339),
			strconv.FormatBool(it.ArchivedAt.Valid)}
		for _, f := range p.Fields {
			row = append(row, it.Fields[f.ID])
		}
		rows = append(rows, row)
	}
	writeCSV(w, p.ID+"-items.csv", rows)
}

func (s *Server) handleExportEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	evs, err := s.DB.AllEvents(p.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := [][]string{{"at", "item_id", "kind", "from_stage", "to_stage", "note"}}
	for _, e := range evs {
		rows = append(rows, []string{e.At.Format(time.RFC3339), strconv.FormatInt(e.ItemID, 10), e.Kind, e.FromStage, e.ToStage, e.Note})
	}
	writeCSV(w, p.ID+"-events.csv", rows)
}

// ---- home and views

type viewBlock struct {
	Pipeline *pipeline.Pipeline
	Result   metrics.Result
	// Compact omits the view's own heading, for pages whose h1 already
	// names it.
	Compact bool
}

type homeData struct {
	Pipelines []pipelineHome
}

type pipelineHome struct {
	Pipeline *pipeline.Pipeline
	Views    []viewBlock
	Retired  map[string]int // stages items sit in that the pipeline no longer has
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	pls, _ := s.loadPipelines()
	var hd homeData
	for _, p := range pls {
		ph := pipelineHome{Pipeline: p}
		for _, v := range p.Views {
			res, err := metrics.Evaluate(p, s.DB, v, s.now())
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			ph.Views = append(ph.Views, viewBlock{Pipeline: p, Result: res})
		}
		counts, err := s.DB.CountsByStage(p.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for id, n := range counts {
			if _, ok := p.Stage(id); !ok {
				if ph.Retired == nil {
					ph.Retired = map[string]int{}
				}
				ph.Retired[id] = n
			}
		}
		hd.Pipelines = append(hd.Pipelines, ph)
	}
	s.render(w, r, "home", "Home", hd)
}

type viewData struct {
	Pipeline *pipeline.Pipeline
	Block    viewBlock
	Part     string
	Title    string
	Items    *itemsTable
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	v, ok := p.View(r.PathValue("vid"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	res, err := metrics.Evaluate(p, s.DB, v, s.now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	part := r.URL.Query().Get("part")
	if part == "" && len(res.Sections) > 0 {
		part = res.Sections[0].Part
	}
	items, title, err := metrics.Items(p, s.DB, v, part, s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, r, "view", v.Label, viewData{
		Pipeline: p,
		Block:    viewBlock{Pipeline: p, Result: res, Compact: true},
		Part:     part,
		Title:    title,
		Items:    s.tableFor(p, items, "", false, "", nil),
	})
}

// ---- items list

type itemsTable struct {
	Pipeline *pipeline.Pipeline
	Items    []dbstore.Item
	Columns  []pipeline.Field
	// Sorting controls are only offered on the full list.
	SortBase string
	Sort     string
	Desc     bool
	Query    string
	Scope    string
	Scopes   []scopeOption
}

type scopeOption struct{ Value, Label string }

func (s *Server) tableFor(p *pipeline.Pipeline, items []dbstore.Item, sortKey string, desc bool, base string, extra map[string]string) *itemsTable {
	t := &itemsTable{Pipeline: p, Items: items, Sort: sortKey, Desc: desc, SortBase: base}
	for _, f := range p.Fields {
		if f.Type != pipeline.FieldLongText {
			t.Columns = append(t.Columns, f)
		}
	}
	return t
}

// SortURL builds the link for a column header: same filters, this sort,
// flipping direction if it is already the sort column.
func (t *itemsTable) SortURL(key string) string {
	q := url.Values{}
	if t.Query != "" {
		q.Set("q", t.Query)
	}
	if t.Scope != "" {
		q.Set("show", t.Scope)
	}
	q.Set("sort", key)
	if t.Sort == key && !t.Desc {
		q.Set("dir", "desc")
	}
	return t.SortBase + "?" + q.Encode()
}

func (t *itemsTable) AriaSort(key string) string {
	if t.Sort != key {
		return ""
	}
	if t.Desc {
		return "descending"
	}
	return "ascending"
}

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	scope := q.Get("show")
	if scope == "" {
		scope = "active"
	}
	f := dbstore.Filter{Pipeline: p.ID, Query: q.Get("q"), IncludeArchived: scope == "archived"}
	switch scope {
	case "active":
		f.StageNot = p.TerminalStages()
	case "finished":
		f.Stages = p.TerminalStages()
	case "all", "archived":
	default:
		if _, ok := p.Stage(scope); ok {
			f.Stages = []string{scope}
		} else {
			f.Stages = []string{scope} // a retired stage: still listable
		}
	}
	sortKey := q.Get("sort")
	if sortKey == "" {
		sortKey = "contact"
	}
	if !p.IsSortKey(sortKey) {
		sortKey = "contact"
	}
	f.Sort, f.Desc = sortKey, q.Get("dir") == "desc"
	if fld, ok := p.Field(sortKey); ok {
		f.SortType = fld.Type
	}
	items, err := s.DB.ListItems(f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	t := s.tableFor(p, items, sortKey, f.Desc, "/p/"+p.ID+"/", nil)
	t.Query, t.Scope = q.Get("q"), scope
	t.Scopes = []scopeOption{{"active", "Active (not finished)"}, {"all", "Everything"}, {"finished", "Finished"}, {"archived", "Including archived"}}
	for _, st := range p.Stages {
		t.Scopes = append(t.Scopes, scopeOption{st.ID, "Only: " + st.Label})
	}
	s.render(w, r, "items", p.Label, t)
}

func (s *Server) handleDefinition(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	s.render(w, r, "definition", p.Label+": definition", p)
}

// ---- new item

type newItemData struct {
	Pipeline *pipeline.Pipeline
	Contacts []dbstore.Contact
	Selected int64
}

func (s *Server) handleNewItem(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	cs, err := s.DB.ListContacts("")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sel, _ := strconv.ParseInt(r.URL.Query().Get("contact"), 10, 64)
	s.render(w, r, "newitem", "Add to "+p.Label, newItemData{Pipeline: p, Contacts: cs, Selected: sel})
}

func fieldsFromForm(p *pipeline.Pipeline, r *http.Request) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range p.Fields {
		v := strings.TrimSpace(r.PostFormValue("f_" + f.ID))
		if v == "" {
			continue
		}
		switch f.Type {
		case pipeline.FieldDate:
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return nil, fmt.Errorf("%s must be a date like 2026-11-04", f.Label)
			}
		case pipeline.FieldNumber:
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return nil, fmt.Errorf("%s must be a number", f.Label)
			}
		}
		out[f.ID] = v
	}
	return out, nil
}

func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	back := "/p/" + p.ID + "/new"
	fields, err := fieldsFromForm(p, r)
	if err != nil {
		redirectError(w, r, back, err.Error())
		return
	}
	var contactID int64
	if sel := r.PostFormValue("contact_id"); sel != "" {
		contactID, err = strconv.ParseInt(sel, 10, 64)
		if err != nil {
			redirectError(w, r, back, "pick a contact from the list or enter a new one")
			return
		}
		if _, err := s.DB.GetContact(contactID); err != nil {
			redirectError(w, r, back, "that contact no longer exists")
			return
		}
	} else {
		c, err := s.DB.CreateContact(r.PostFormValue("name"), r.PostFormValue("email"), r.PostFormValue("website"), "")
		if err != nil {
			redirectError(w, r, back, "to add someone new, enter at least a name ("+err.Error()+")")
			return
		}
		contactID = c.ID
	}
	it, err := s.DB.CreateItem(p.ID, contactID, p.FirstStage().ID, fields)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectNotice(w, r, fmt.Sprintf("/p/%s/items/%d", p.ID, it.ID), "Added.")
}

// ---- one item

type renderedTemplate struct {
	Name    string
	Text    string
	Missing []string
}

type itemData struct {
	Pipeline  *pipeline.Pipeline
	Item      dbstore.Item
	Stage     pipeline.Stage
	Retired   bool
	Next      *pipeline.Stage
	Exits     []pipeline.Stage
	All       []pipeline.Stage
	Drafts    []renderedTemplate
	History   []dbstore.Event
	Contact   dbstore.Contact
	Waiting   string
	FieldList []pipeline.Field
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return
	}
	id, ok := idParam(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	it, err := s.DB.GetItem(id)
	if errors.Is(err, dbstore.ErrNotFound) || (err == nil && it.Pipeline != p.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	c, err := s.DB.GetContact(it.ContactID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	hist, err := s.DB.ItemEvents(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := itemData{Pipeline: p, Item: it, History: hist, Contact: c, All: p.Stages, FieldList: p.Fields}
	st, known := p.Stage(it.Stage)
	if !known {
		d.Retired = true
		st = pipeline.Stage{ID: it.Stage, Label: it.Stage}
	}
	d.Stage = st
	d.Waiting = st.WaitingOn()
	if st.Next != "" {
		if n, ok := p.Stage(st.Next); ok {
			d.Next = &n
		}
	}
	for _, x := range st.Exits {
		if e, ok := p.Stage(x); ok {
			d.Exits = append(d.Exits, e)
		}
	}
	for _, name := range st.Templates {
		text, missing, err := p.Render(name, c.Map(), it.Fields, st.ID, s.now())
		if err != nil {
			text = "This template could not be filled in: " + err.Error()
		}
		d.Drafts = append(d.Drafts, renderedTemplate{Name: name, Text: text, Missing: missing})
	}
	s.render(w, r, "item", c.Name+": "+p.Label, d)
}

func (s *Server) itemFor(w http.ResponseWriter, r *http.Request) (*pipeline.Pipeline, dbstore.Item, bool) {
	p, ok := s.pipelineFor(w, r)
	if !ok {
		return nil, dbstore.Item{}, false
	}
	id, ok := idParam(r, "id")
	if !ok {
		http.NotFound(w, r)
		return nil, dbstore.Item{}, false
	}
	it, err := s.DB.GetItem(id)
	if err != nil || it.Pipeline != p.ID {
		http.NotFound(w, r)
		return nil, dbstore.Item{}, false
	}
	return p, it, true
}

func (s *Server) handleItemFields(w http.ResponseWriter, r *http.Request) {
	p, it, ok := s.itemFor(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/p/%s/items/%d", p.ID, it.ID)
	fields, err := fieldsFromForm(p, r)
	if err != nil {
		redirectError(w, r, back, err.Error())
		return
	}
	if err := s.DB.UpdateItemFields(it.ID, fields); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirectNotice(w, r, back, "Saved.")
}

func (s *Server) handleItemMove(w http.ResponseWriter, r *http.Request) {
	p, it, ok := s.itemFor(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/p/%s/items/%d", p.ID, it.ID)
	to := r.PostFormValue("to")
	if to == "" {
		to = r.PostFormValue("to_any")
	}
	st, ok := p.Stage(to)
	if !ok {
		redirectError(w, r, back, "that stage does not exist in this pipeline")
		return
	}
	if it.Stage == st.ID {
		redirectError(w, r, back, "It is already at "+st.Label+".")
		return
	}
	if err := s.DB.MoveItem(it.ID, st.ID, strings.TrimSpace(r.PostFormValue("note"))); err != nil {
		redirectError(w, r, back, err.Error())
		return
	}
	redirectNotice(w, r, back, "Moved to "+st.Label+".")
}

func (s *Server) handleItemArchive(w http.ResponseWriter, r *http.Request) {
	p, it, ok := s.itemFor(w, r)
	if !ok {
		return
	}
	archive := r.PostFormValue("archive") == "1"
	if err := s.DB.SetArchived(it.ID, archive); err != nil {
		s.serverError(w, r, err)
		return
	}
	if archive {
		redirectNotice(w, r, "/p/"+p.ID+"/", "Archived. It no longer shows in lists or counts; its history is kept.")
		return
	}
	redirectNotice(w, r, fmt.Sprintf("/p/%s/items/%d", p.ID, it.ID), "Restored.")
}

// ---- contacts

type contactsData struct {
	Contacts []dbstore.Contact
	Query    string
}

func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	cs, err := s.DB.ListContacts(q)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "contacts", "Contacts", contactsData{Contacts: cs, Query: q})
}

func (s *Server) handleNewContact(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "newcontact", "New contact", nil)
}

func (s *Server) handleCreateContact(w http.ResponseWriter, r *http.Request) {
	c, err := s.DB.CreateContact(r.PostFormValue("name"), r.PostFormValue("email"), r.PostFormValue("website"), r.PostFormValue("notes"))
	if err != nil {
		redirectError(w, r, "/contacts/new", err.Error())
		return
	}
	redirectNotice(w, r, fmt.Sprintf("/contacts/%d", c.ID), "Contact added.")
}

type contactData struct {
	Contact   dbstore.Contact
	Pipelines []contactItems
}

type contactItems struct {
	Pipeline *pipeline.Pipeline
	Items    []dbstore.Item
}

func (s *Server) handleContact(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	c, err := s.DB.GetContact(id)
	if errors.Is(err, dbstore.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pls, _ := s.loadPipelines()
	d := contactData{Contact: c}
	for _, p := range pls {
		items, err := s.DB.ListItems(dbstore.Filter{Pipeline: p.ID, ContactID: id, Sort: "created"})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
		d.Pipelines = append(d.Pipelines, contactItems{Pipeline: p, Items: items})
	}
	s.render(w, r, "contact", c.Name, d)
}

func (s *Server) handleUpdateContact(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := fmt.Sprintf("/contacts/%d", id)
	err := s.DB.UpdateContact(id, r.PostFormValue("name"), r.PostFormValue("email"), r.PostFormValue("website"), r.PostFormValue("notes"))
	if errors.Is(err, dbstore.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		redirectError(w, r, back, err.Error())
		return
	}
	redirectNotice(w, r, back, "Saved.")
}

// ---- definitions sync

type syncPipeline struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Stages int    `json:"stages"`
	Fields int    `json:"fields"`
	Views  int    `json:"views"`
}

// handleAdminSync installs a new set of definitions from a gzipped tar
// sent by CI. It is the only way definitions change on a running app, and
// a set that does not pass the checker is refused before anything the app
// is serving is touched.
func (s *Server) handleAdminSync(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if s.SyncToken == "" || s.DataDir == "" {
		reply(http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "sync is not configured on this server"})
		return
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix ||
		subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.SyncToken)) != 1 {
		reply(http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	res, err := defs.Install(s.DataDir, r.Body)
	var (
		pe  *defs.ProblemsError
		ae  *defs.ArchiveError
		mbe *http.MaxBytesError
	)
	switch {
	case errors.As(err, &mbe):
		reply(http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "the upload is larger than 5 MiB"})
		return
	case errors.As(err, &pe):
		log.Printf("sync refused: %d problem(s)", len(pe.Problems))
		reply(http.StatusUnprocessableEntity, map[string]any{"ok": false, "problems": pe.Problems})
		return
	case errors.As(err, &ae):
		reply(http.StatusBadRequest, map[string]any{"ok": false, "error": ae.Error()})
		return
	case err != nil:
		log.Printf("sync error: %v", err)
		reply(http.StatusInternalServerError, map[string]any{"ok": false, "error": "could not install the definitions; see the server log"})
		return
	}

	out := map[string]any{"ok": true, "release": res.Release}
	var pls []syncPipeline
	var warnings []string
	for _, p := range res.Pipelines {
		pls = append(pls, syncPipeline{p.ID, p.Label, len(p.Stages), len(p.Fields), len(p.Views)})
		// Items already sitting in a stage this version no longer has are
		// not lost; say so, so it is not a surprise on the home page.
		if counts, err := s.DB.CountsByStage(p.ID); err == nil {
			var gone []string
			for id := range counts {
				if _, ok := p.Stage(id); !ok {
					gone = append(gone, id)
				}
			}
			sort.Strings(gone)
			for _, id := range gone {
				warnings = append(warnings, fmt.Sprintf(
					"%s: %d item(s) are in the stage %q, which this version no longer has. They show on Home until moved to a current stage.",
					p.ID, counts[id], id))
			}
		}
	}
	out["pipelines"] = pls
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	log.Printf("sync installed release %s (%d pipeline(s))", res.Release, len(pls))
	reply(http.StatusOK, out)
}
