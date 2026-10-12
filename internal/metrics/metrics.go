// Package metrics evaluates a pipeline's views against the database. Every
// number it produces comes with a Part, and Items(view, part) returns the
// exact items behind it, so any number can be opened.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitea.reground.org/will/pipelines/internal/dbstore"
	"gitea.reground.org/will/pipelines/internal/pipeline"
)

// Section is one number on a view.
type Section struct {
	Label string
	Count int
	Note  string
	Part  string // pass to Items to get the entries behind Count
}

type Result struct {
	View     pipeline.View
	Sections []Section
	Note     string // shown under the view's title
}

// FilterFor turns a view's where clause into a database filter.
func FilterFor(p *pipeline.Pipeline, w pipeline.Where) dbstore.Filter {
	f := dbstore.Filter{Pipeline: p.ID}
	stages := append([]string(nil), w.Stage...)
	if w.WaitingOn != "" {
		waiting := p.StagesWaitingOn(w.WaitingOn)
		if len(stages) > 0 {
			stages = intersect(stages, waiting)
		} else {
			stages = waiting
		}
		if len(stages) == 0 {
			stages = []string{"\x00none"} // matches nothing
		}
	}
	f.Stages = stages
	f.StageNot = append([]string(nil), w.StageNot...)
	if w.Active != nil {
		terminals := p.TerminalStages()
		if *w.Active {
			f.StageNot = append(f.StageNot, terminals...)
		} else {
			if len(f.Stages) > 0 {
				f.Stages = intersect(f.Stages, terminals)
			} else {
				f.Stages = terminals
			}
			if len(f.Stages) == 0 {
				f.Stages = []string{"\x00none"}
			}
		}
	}
	if w.DueWithin != nil {
		d := w.DueWithin.Days
		f.DueField, f.DueDays = w.DueWithin.Field, &d
	}
	f.DaysInStageOver = w.DaysInStageOver
	return f
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
}

// Evaluate computes a view's numbers as of now.
func Evaluate(p *pipeline.Pipeline, db *dbstore.DB, v pipeline.View, now time.Time) (Result, error) {
	res := Result{View: v}
	switch v.Kind {
	case pipeline.KindList:
		items, err := db.ListItems(FilterFor(p, v.Where))
		if err != nil {
			return res, err
		}
		res.Sections = []Section{{Label: plural(len(items), "item"), Count: len(items), Part: "all"}}

	case pipeline.KindCounts:
		counts, err := db.CountsByStage(p.ID)
		if err != nil {
			return res, err
		}
		known := map[string]bool{}
		for _, s := range p.Stages {
			known[s.ID] = true
			res.Sections = append(res.Sections, Section{Label: s.Label, Count: counts[s.ID], Part: "stage:" + s.ID})
		}
		var retired []string
		for id := range counts {
			if !known[id] {
				retired = append(retired, id)
			}
		}
		sort.Strings(retired)
		for _, id := range retired {
			res.Sections = append(res.Sections, Section{
				Label: id + " (no longer in the pipeline)", Count: counts[id], Part: "stage:" + id,
				Note: "These items are in a stage that has been removed from the pipeline file. Open one and move it to a current stage.",
			})
		}

	case pipeline.KindEvents:
		if v.Event.Repeated != "" {
			times, err := db.RepeatTimes(p.ID, v.Event.Repeated)
			if err != nil {
				return res, err
			}
			for _, w := range v.Windows {
				n, ids := repeatsBetween(times, now.AddDate(0, 0, -w), now.Add(time.Second))
				nPrev, _ := repeatsBetween(times, now.AddDate(0, 0, -2*w), now.AddDate(0, 0, -w))
				res.Sections = append(res.Sections, Section{
					Label: fmt.Sprintf("Last %d days", w),
					Count: n,
					Note: fmt.Sprintf("Across %s. %.2f per day. The %d days before that: %d.",
						plural(len(ids), "entry"), float64(n)/float64(w), w, nPrev),
					Part: "w" + strconv.Itoa(w),
				})
			}
			break
		}
		first, err := db.FirstEntries(p.ID, v.Event.Entered)
		if err != nil {
			return res, err
		}
		for _, w := range v.Windows {
			cur := countBetween(first, now.AddDate(0, 0, -w), now.Add(time.Second))
			prev := countBetween(first, now.AddDate(0, 0, -2*w), now.AddDate(0, 0, -w))
			res.Sections = append(res.Sections, Section{
				Label: fmt.Sprintf("Last %d days", w),
				Count: len(cur),
				Note: fmt.Sprintf("%.2f per day. The %d days before that: %d.",
					float64(len(cur))/float64(w), w, len(prev)),
				Part: "w" + strconv.Itoa(w),
			})
		}

	case pipeline.KindCohort:
		cohort, reached, pending, start, end, err := cohortSets(p, db, v, now)
		if err != nil {
			return res, err
		}
		res.Note = fmt.Sprintf("Items first entering %s from %s to %s.",
			stageLabels(p, v.Event.Entered), start.Format("Jan 2"), end.Format("Jan 2"))
		pct := "no items in this period yet"
		if len(cohort) > 0 {
			pct = fmt.Sprintf("%.0f%% of %d", 100*float64(len(reached))/float64(len(cohort)), len(cohort))
		}
		res.Sections = []Section{
			{Label: "In the group", Count: len(cohort), Part: "cohort"},
			{Label: "Reached " + stageLabels(p, v.Outcome.Entered), Count: len(reached), Note: pct, Part: "outcome"},
			{Label: "Still waiting", Count: len(pending), Part: "pending"},
		}
	default:
		return res, fmt.Errorf("unknown view kind %q", v.Kind)
	}
	return res, nil
}

// Items returns the entries behind one number of a view, with a title
// describing what they are.
func Items(p *pipeline.Pipeline, db *dbstore.DB, v pipeline.View, part string, now time.Time) ([]dbstore.Item, string, error) {
	byIDs := func(ids []int64) ([]dbstore.Item, error) {
		if len(ids) == 0 {
			return nil, nil
		}
		return db.ListItems(dbstore.Filter{Pipeline: p.ID, IDs: ids, Sort: "entered"})
	}
	switch v.Kind {
	case pipeline.KindList:
		f := FilterFor(p, v.Where)
		if v.Sort != "" {
			f.Sort, f.Desc = v.Sort, v.Desc
			if fld, ok := p.Field(v.Sort); ok {
				f.SortType = fld.Type
			}
		}
		items, err := db.ListItems(f)
		return items, v.Label, err

	case pipeline.KindCounts:
		stage := strings.TrimPrefix(part, "stage:")
		if stage == part || stage == "" {
			return nil, "", fmt.Errorf("unknown part %q", part)
		}
		items, err := db.ListItems(dbstore.Filter{Pipeline: p.ID, Stages: []string{stage}, Sort: "entered"})
		label := stage
		if s, ok := p.Stage(stage); ok {
			label = s.Label
		}
		return items, v.Label + ": " + label, err

	case pipeline.KindEvents:
		w, err := strconv.Atoi(strings.TrimPrefix(part, "w"))
		if err != nil || !strings.HasPrefix(part, "w") {
			return nil, "", fmt.Errorf("unknown part %q", part)
		}
		if v.Event.Repeated != "" {
			times, err := db.RepeatTimes(p.ID, v.Event.Repeated)
			if err != nil {
				return nil, "", err
			}
			_, ids := repeatsBetween(times, now.AddDate(0, 0, -w), now.Add(time.Second))
			items, err := byIDs(ids)
			return items, fmt.Sprintf("%s: last %d days", v.Label, w), err
		}
		first, err := db.FirstEntries(p.ID, v.Event.Entered)
		if err != nil {
			return nil, "", err
		}
		items, err := byIDs(countBetween(first, now.AddDate(0, 0, -w), now.Add(time.Second)))
		return items, fmt.Sprintf("%s: last %d days", v.Label, w), err

	case pipeline.KindCohort:
		cohort, reached, pending, _, _, err := cohortSets(p, db, v, now)
		if err != nil {
			return nil, "", err
		}
		var ids []int64
		var name string
		switch part {
		case "cohort":
			ids, name = cohort, "in the group"
		case "outcome":
			ids, name = reached, "reached "+stageLabels(p, v.Outcome.Entered)
		case "pending":
			ids, name = pending, "still waiting"
		default:
			return nil, "", fmt.Errorf("unknown part %q", part)
		}
		items, err := byIDs(ids)
		return items, v.Label + ": " + name, err
	}
	return nil, "", fmt.Errorf("unknown view kind %q", v.Kind)
}

func countBetween(m map[int64]time.Time, from, to time.Time) []int64 {
	var ids []int64
	for id, at := range m {
		if !at.Before(from) && at.Before(to) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// repeatsBetween counts every occurrence in [from, to) and returns the
// distinct items they belong to. Unlike countBetween, an item that did the
// action three times in the window counts three times.
func repeatsBetween(m map[int64][]time.Time, from, to time.Time) (int, []int64) {
	n := 0
	var ids []int64
	for id, ts := range m {
		hit := false
		for _, at := range ts {
			if !at.Before(from) && at.Before(to) {
				n++
				hit = true
			}
		}
		if hit {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return n, ids
}

// cohortSets splits the cohort into those that reached the outcome after
// entering it, and those that have not (yet).
func cohortSets(p *pipeline.Pipeline, db *dbstore.DB, v pipeline.View, now time.Time) (cohort, reached, pending []int64, start, end time.Time, err error) {
	end = now.AddDate(0, 0, -v.EndingDaysAgo)
	start = end.AddDate(0, 0, -v.WindowDays)
	entered, err := db.FirstEntries(p.ID, v.Event.Entered)
	if err != nil {
		return
	}
	outcome, err := db.FirstEntries(p.ID, v.Outcome.Entered)
	if err != nil {
		return
	}
	cohort = countBetween(entered, start, end)
	for _, id := range cohort {
		if at, ok := outcome[id]; ok && !at.Before(entered[id]) {
			reached = append(reached, id)
		} else {
			pending = append(pending, id)
		}
	}
	return
}

func stageLabels(p *pipeline.Pipeline, ids []string) string {
	var out []string
	for _, id := range ids {
		if s, ok := p.Stage(id); ok {
			out = append(out, s.Label)
		} else {
			out = append(out, id)
		}
	}
	return strings.Join(out, " or ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
