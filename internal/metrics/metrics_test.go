package metrics

import (
	"fmt"
	"testing"
	"time"

	"gitea.reground.org/will/pipelines/internal/dbstore"
	"gitea.reground.org/will/pipelines/internal/pipeline"
	"gitea.reground.org/will/pipelines/internal/testdb"
)

type fixture struct {
	t  *testing.T
	db *dbstore.DB
	p  *pipeline.Pipeline
	c  dbstore.Contact
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := testdb.New(t)
	p, err := pipeline.Load("../pipeline/testdata/basic")
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.CreateContact("Test Person", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, db: db, p: p, c: c}
}

// item creates an item sitting at stage, with its history rewritten so
// that it entered that stage daysAgo days ago.
func (f *fixture) item(stage string, daysAgo int, fields map[string]string) int64 {
	f.t.Helper()
	it, err := f.db.CreateItem(f.p.ID, f.c.ID, "start", fields)
	if err != nil {
		f.t.Fatal(err)
	}
	if stage != "start" {
		f.enter(it.ID, stage, daysAgo)
		_, err := f.db.Exec(`UPDATE items SET stage=$2, stage_entered_at = now() - ($3 * interval '1 day') WHERE id=$1`, it.ID, stage, daysAgo)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return it.ID
}

// enter records an entry into a stage daysAgo days ago.
func (f *fixture) enter(id int64, stage string, daysAgo int) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO events (item_id, pipeline, kind, to_stage, at)
		VALUES ($1,$2,'moved',$3, now() - ($4 * interval '1 day'))`, id, f.p.ID, stage, daysAgo)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) view(v pipeline.View) Result {
	f.t.Helper()
	r, err := Evaluate(f.p, f.db, v, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func counts(r Result) map[string]int {
	m := map[string]int{}
	for _, s := range r.Sections {
		m[s.Part] = s.Count
	}
	return m
}

func TestEventsCountFirstEntryPerItemInWindow(t *testing.T) {
	f := setup(t)
	a := f.item("wait_a", 3, nil)
	f.item("wait_a", 20, nil)
	c := f.item("wait_a", 3, nil)
	f.enter(c, "wait_a", 1) // entered again later: still one pitch
	_ = a
	r := f.view(pipeline.View{ID: "e", Kind: pipeline.KindEvents,
		Event: &pipeline.EnteredSpec{Entered: pipeline.StringList{"wait_a"}}, Windows: []int{7, 30}})
	got := counts(r)
	if got["w7"] != 2 || got["w30"] != 3 {
		t.Fatalf("got %v, want w7=2 w30=3", got)
	}
}

func TestEventsArePerWindowNotCurrentState(t *testing.T) {
	f := setup(t)
	// Entered wait_a 2 days ago and has since moved on: still counts as a
	// pitch even though it no longer sits in wait_a.
	id := f.item("finish", 1, nil)
	f.enter(id, "wait_a", 2)
	r := f.view(pipeline.View{ID: "e", Kind: pipeline.KindEvents,
		Event: &pipeline.EnteredSpec{Entered: pipeline.StringList{"wait_a"}}, Windows: []int{7}})
	if got := counts(r)["w7"]; got != 1 {
		t.Fatalf("an item that moved on should still count, got %d", got)
	}
}

func TestCohortReplyRate(t *testing.T) {
	f := setup(t)
	replied := f.item("finish", 5, nil)
	f.enter(replied, "wait_a", 20)
	declined := f.item("no", 10, nil)
	f.enter(declined, "wait_a", 25)
	f.item("wait_a", 30, nil) // still waiting, in the cohort
	f.item("wait_a", 3, nil)  // too recent: outside the cohort window
	f.item("wait_a", 60, nil) // too old: outside the cohort window
	r := f.view(pipeline.View{ID: "r", Kind: pipeline.KindCohort,
		Event:      &pipeline.EnteredSpec{Entered: pipeline.StringList{"wait_a"}},
		Outcome:    &pipeline.EnteredSpec{Entered: pipeline.StringList{"finish", "no"}},
		WindowDays: 30, EndingDaysAgo: 14})
	got := counts(r)
	if got["cohort"] != 3 || got["outcome"] != 2 || got["pending"] != 1 {
		t.Fatalf("got %v, want cohort=3 outcome=2 pending=1", got)
	}
	items, _, err := Items(f.p, f.db, pipeline.View{ID: "r", Kind: pipeline.KindCohort,
		Event:      &pipeline.EnteredSpec{Entered: pipeline.StringList{"wait_a"}},
		Outcome:    &pipeline.EnteredSpec{Entered: pipeline.StringList{"finish", "no"}},
		WindowDays: 30, EndingDaysAgo: 14}, "pending", time.Now())
	if err != nil || len(items) != 1 {
		t.Fatalf("drill-down for pending: %v %d", err, len(items))
	}
}

func TestListFilters(t *testing.T) {
	f := setup(t)
	soon := time.Now().AddDate(0, 0, 5).Format("2006-01-02")
	late := time.Now().AddDate(0, 0, 40).Format("2006-01-02")
	f.item("start", 0, map[string]string{"due": soon})   // mine, due soon
	f.item("wait_a", 12, map[string]string{"due": late}) // theirs, stuck 12 days
	f.item("finish", 1, nil)                             // finished

	n := func(w pipeline.Where) int {
		items, err := f.db.ListItems(FilterFor(f.p, w))
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	yes := true
	cases := []struct {
		name string
		w    pipeline.Where
		want int
	}{
		{"waiting on me", pipeline.Where{WaitingOn: "me"}, 1},
		{"waiting on them", pipeline.Where{WaitingOn: "them"}, 1},
		{"active", pipeline.Where{Active: &yes}, 2},
		{"due within 14", pipeline.Where{DueWithin: &pipeline.DueWithin{Field: "due", Days: 14}}, 1},
		{"due within 60", pipeline.Where{DueWithin: &pipeline.DueWithin{Field: "due", Days: 60}}, 2},
		{"stuck over 10 days", pipeline.Where{DaysInStageOver: 10}, 1},
		{"stage not", pipeline.Where{StageNot: pipeline.StringList{"start"}}, 2},
		{"waiting on me and active", pipeline.Where{WaitingOn: "me", Active: &yes}, 1},
	}
	for _, c := range cases {
		if got := n(c.w); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestCountsShowRetiredStages(t *testing.T) {
	f := setup(t)
	f.item("start", 0, nil)
	f.item("old_stage_that_was_removed", 2, nil)
	r := f.view(pipeline.View{ID: "c", Kind: pipeline.KindCounts})
	got := counts(r)
	if got["stage:start"] != 1 || got["stage:old_stage_that_was_removed"] != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestArchivedItemsLeaveTheNumbers(t *testing.T) {
	f := setup(t)
	id := f.item("wait_a", 2, nil)
	if err := f.db.SetArchived(id, true); err != nil {
		t.Fatal(err)
	}
	r := f.view(pipeline.View{ID: "e", Kind: pipeline.KindEvents,
		Event: &pipeline.EnteredSpec{Entered: pipeline.StringList{"wait_a"}}, Windows: []int{7}})
	if got := counts(r)["w7"]; got != 0 {
		t.Fatalf("archived item should not count, got %d", got)
	}
}

func TestEventLogIsAppendOnly(t *testing.T) {
	f := setup(t)
	f.item("start", 0, nil)
	if _, err := f.db.Exec(`UPDATE events SET note='x'`); err == nil {
		t.Fatal("update should be refused")
	}
	if _, err := f.db.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("delete should be refused")
	}
	_ = fmt.Sprint
}

// repeat records a repeat action at the item's current stage, daysAgo days ago.
func (f *fixture) repeat(id int64, stage string, daysAgo int) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO events (item_id, pipeline, kind, from_stage, to_stage, repeat_id, at)
		VALUES ($1,$2,'repeated',$3,$3,'nudge', now() - ($4 * interval '1 day'))`, id, f.p.ID, stage, daysAgo)
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestRepeatedEventsCountEveryOccurrence(t *testing.T) {
	f := setup(t)
	a := f.item("wait_a", 20, nil)
	f.repeat(a, "wait_a", 12)
	f.repeat(a, "wait_a", 5)
	f.repeat(a, "wait_a", 2)
	b := f.item("wait_a", 10, nil)
	f.repeat(b, "wait_a", 1)
	f.item("wait_a", 10, nil) // never nudged

	v := pipeline.View{ID: "n", Kind: pipeline.KindEvents,
		Event: &pipeline.EnteredSpec{Repeated: "nudge"}, Windows: []int{7, 30}}
	got := counts(f.view(v))
	if got["w7"] != 3 || got["w30"] != 4 {
		t.Fatalf("got %v, want w7=3 (two by a, one by b) and w30=4", got)
	}
	items, _, err := Items(f.p, f.db, v, "w7", time.Now())
	if err != nil || len(items) != 2 {
		t.Fatalf("drill-down should list the 2 items nudged this week: %v %d", err, len(items))
	}

	// Repeating does not make an item enter its stage again.
	first := f.view(pipeline.View{ID: "e", Kind: pipeline.KindEvents,
		Event: &pipeline.EnteredSpec{Entered: pipeline.StringList{"wait_a"}}, Windows: []int{30}})
	if got := counts(first)["w30"]; got != 3 {
		t.Fatalf("pitches should still count 3 items once each, got %d", got)
	}
}
