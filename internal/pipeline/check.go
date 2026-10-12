package pipeline

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Check validates a pipeline and returns every problem found, written so a
// person who has never seen the engine can fix them. Warnings are things
// worth knowing that do not stop the pipeline from running.
func (p *Pipeline) Check() (problems, warnings []string) {
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	warn := func(format string, a ...any) { warnings = append(warnings, fmt.Sprintf(format, a...)) }

	if !idRe.MatchString(p.ID) {
		bad("the pipeline id %q must be lowercase letters, digits and underscores, starting with a letter", p.ID)
	}
	if strings.TrimSpace(p.Label) == "" {
		bad("the pipeline needs a label")
	}

	// Fields.
	fieldIDs := map[string]Field{}
	for _, f := range p.Fields {
		switch {
		case !idRe.MatchString(f.ID):
			bad("field id %q must be lowercase letters, digits and underscores, starting with a letter", f.ID)
		}
		if _, dup := fieldIDs[f.ID]; dup {
			bad("field %q is defined more than once", f.ID)
		}
		fieldIDs[f.ID] = f
		switch f.Type {
		case FieldText, FieldLongText, FieldNumber, FieldDate, FieldURL:
		default:
			bad("field %q has type %q; use one of: text, longtext, number, date, url", f.ID, f.Type)
		}
		if strings.TrimSpace(f.Label) == "" {
			bad("field %q needs a label", f.ID)
		}
	}

	// Stages.
	if len(p.Stages) == 0 {
		bad("the pipeline has no stages")
		return problems, warnings
	}
	stageIDs := map[string]Stage{}
	for _, s := range p.Stages {
		if !idRe.MatchString(s.ID) {
			bad("stage id %q must be lowercase letters, digits and underscores, starting with a letter", s.ID)
		}
		if _, dup := stageIDs[s.ID]; dup {
			bad("stage %q is defined more than once", s.ID)
		}
		stageIDs[s.ID] = s
	}
	hasTerminal := false
	for _, s := range p.Stages {
		name := fmt.Sprintf("stage %q", s.ID)
		if strings.TrimSpace(s.Label) == "" {
			bad("%s needs a label", name)
		}
		switch s.Type {
		case TypeDo, TypeDraft, TypeSend, TypeWait:
		default:
			bad("%s has type %q; use one of: do, draft, send, wait", name, s.Type)
		}
		switch s.Waiting {
		case "", "me", "them", "none":
		default:
			bad("%s has waiting_on %q; use me, them, or none", name, s.Waiting)
		}
		if s.Terminal {
			hasTerminal = true
			if s.Next != "" || len(s.Exits) > 0 {
				bad("%s is marked terminal (finished) but also has a next step or exits; finished stages cannot lead anywhere", name)
			}
		} else if s.Next == "" && len(s.Exits) == 0 {
			bad("%s has nowhere to go. Give it a next step, or mark it terminal: true if items finish here", name)
		}
		if s.Next != "" {
			if _, ok := stageIDs[s.Next]; !ok {
				bad("%s says its next step is %q, which is not a stage in this pipeline", name, s.Next)
			}
			if s.Next == s.ID {
				bad("%s lists itself as its next step", name)
			}
		}
		for _, x := range s.Exits {
			if _, ok := stageIDs[x]; !ok {
				bad("%s lists an exit to %q, which is not a stage in this pipeline", name, x)
			}
			if x == s.ID {
				bad("%s lists itself as an exit", name)
			}
			if x == s.Next {
				warn("%s lists %q as both its next step and an exit", name, x)
			}
		}
		for _, t := range s.Templates {
			if _, ok := p.Templates[t]; !ok {
				bad("%s uses the template %q, but there is no file templates/%s", name, t, t)
			}
		}
	}
	if !hasTerminal {
		bad("no stage is marked terminal: true, so no item can ever finish")
	}

	// Reachability from the first stage.
	reach := map[string]bool{p.Stages[0].ID: true}
	queue := []string{p.Stages[0].ID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		s := stageIDs[cur]
		for _, n := range append([]string{s.Next}, s.Exits...) {
			if n != "" && !reach[n] {
				if _, ok := stageIDs[n]; ok {
					reach[n] = true
					queue = append(queue, n)
				}
			}
		}
	}
	var unreachable []string
	for _, s := range p.Stages {
		if !reach[s.ID] {
			unreachable = append(unreachable, s.ID)
		}
	}
	if len(unreachable) > 0 {
		hint := ""
		if len(problems) > 0 {
			hint = " This is usually caused by another problem listed here, such as a next step or exit with a typo."
		}
		bad("these stages can never be reached from the first stage (%s): %s.%s",
			p.Stages[0].ID, strings.Join(unreachable, ", "), hint)
	}

	// Templates: parse, and catch field names that do not exist.
	used := map[string]bool{}
	for _, s := range p.Stages {
		for _, t := range s.Templates {
			used[t] = true
		}
	}
	names := make([]string, 0, len(p.Templates))
	for n := range p.Templates {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := p.checkTemplate(n); err != nil {
			var ids []string
			for _, f := range p.Fields {
				ids = append(ids, f.ID)
			}
			bad("template %q has a problem: %v. Templates may use .Contact.(%s) and .Item.(%s)",
				n, cleanTemplateErr(err), strings.Join(ContactKeys, " / "), strings.Join(ids, " / "))
		}
		if !used[n] {
			warn("template %q is not used by any stage", n)
		}
	}

	// Views.
	seenView := map[string]bool{}
	for _, v := range p.Views {
		name := fmt.Sprintf("view %q", v.ID)
		if !idRe.MatchString(v.ID) {
			bad("view id %q must be lowercase letters, digits and underscores, starting with a letter", v.ID)
		}
		if seenView[v.ID] {
			bad("%s is defined more than once", name)
		}
		seenView[v.ID] = true
		if strings.TrimSpace(v.Label) == "" {
			bad("%s needs a label", name)
		}
		stageRef := func(where, id string) {
			if _, ok := stageIDs[id]; !ok {
				bad("%s refers to the stage %q in %s, which is not a stage in this pipeline", name, id, where)
			}
		}
		switch v.Kind {
		case KindList:
			for _, id := range v.Where.Stage {
				stageRef("where.stage", id)
			}
			for _, id := range v.Where.StageNot {
				stageRef("where.stage_not", id)
			}
			if w := v.Where.WaitingOn; w != "" && w != "me" && w != "them" {
				bad("%s: where.waiting_on must be me or them, not %q", name, w)
			}
			if d := v.Where.DueWithin; d != nil {
				f, ok := fieldIDs[d.Field]
				switch {
				case !ok:
					bad("%s: due_within refers to the field %q, which does not exist", name, d.Field)
				case f.Type != FieldDate:
					bad("%s: due_within needs a date field, but %q is %s", name, d.Field, f.Type)
				}
				if d.Days < 0 {
					bad("%s: due_within days cannot be negative", name)
				}
			}
			if v.Sort != "" && !isSortKey(p, v.Sort) {
				bad("%s sorts by %q; use contact, stage, entered, created, or a field id", name, v.Sort)
			}
		case KindCounts:
			// counts per stage; nothing further to configure
		case KindEvents:
			if v.Event == nil || len(v.Event.Entered) == 0 {
				bad("%s needs event: {entered: <stage>}", name)
			} else {
				for _, id := range v.Event.Entered {
					stageRef("event.entered", id)
				}
			}
			if len(v.Windows) == 0 {
				bad("%s needs windows: a list of day counts, e.g. [7, 30, 90]", name)
			}
			for _, w := range v.Windows {
				if w <= 0 {
					bad("%s has a window of %d days; windows must be at least 1", name, w)
				}
			}
		case KindCohort:
			if v.Event == nil || len(v.Event.Entered) == 0 {
				bad("%s needs event: {entered: <stage>} to say who is in the cohort", name)
			} else {
				for _, id := range v.Event.Entered {
					stageRef("event.entered", id)
				}
			}
			if v.Outcome == nil || len(v.Outcome.Entered) == 0 {
				bad("%s needs outcome: {entered: [<stage>, ...]} to say what counts as a result", name)
			} else {
				for _, id := range v.Outcome.Entered {
					stageRef("outcome.entered", id)
				}
			}
			if v.WindowDays <= 0 {
				bad("%s needs window_days of at least 1", name)
			}
			if v.EndingDaysAgo < 0 {
				bad("%s: ending_days_ago cannot be negative", name)
			}
		default:
			bad("%s has kind %q; use one of: list, counts, events, cohort", name, v.Kind)
		}
	}
	return problems, warnings
}

// SortKeys that are not fields.
var builtinSorts = map[string]bool{"contact": true, "stage": true, "entered": true, "created": true}

func isSortKey(p *Pipeline, k string) bool {
	if builtinSorts[k] {
		return true
	}
	_, ok := p.Field(k)
	return ok
}

// IsSortKey reports whether k is something items can be sorted by.
func (p *Pipeline) IsSortKey(k string) bool { return isSortKey(p, k) }

// cleanTemplateErr trims text/template's boilerplate from an error.
func cleanTemplateErr(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "template: ")
	return s
}
