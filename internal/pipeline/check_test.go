package pipeline

import (
	"strings"
	"testing"
	"time"
)

func load(t *testing.T) *Pipeline {
	t.Helper()
	p, err := Load("testdata/basic")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFixtureIsValid(t *testing.T) {
	problems, warnings := load(t).Check()
	if len(problems) > 0 || len(warnings) > 0 {
		t.Fatalf("problems=%v warnings=%v", problems, warnings)
	}
}

func TestProblemsArePlainEnglish(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Pipeline)
		want   string
	}{
		{"dangling next", func(p *Pipeline) { p.Stages[0].Next = "nowhere" }, `"nowhere", which is not a stage`},
		{"dangling exit", func(p *Pipeline) { p.Stages[1].Exits = []string{"gone"} }, `exit to "gone"`},
		{"terminal with next", func(p *Pipeline) { p.Stages[2].Next = "start" }, "cannot lead anywhere"},
		{"dead end", func(p *Pipeline) { p.Stages[0].Next = "" }, "has nowhere to go"},
		{"bad type", func(p *Pipeline) { p.Stages[0].Type = "think" }, `has type "think"`},
		{"duplicate stage", func(p *Pipeline) { p.Stages[3].ID = "finish" }, "more than once"},
		{"unreachable", func(p *Pipeline) { p.Stages[1].Next = "no"; p.Stages[1].Exits = nil }, "can never be reached"},
		{"no terminal", func(p *Pipeline) {
			p.Stages[2].Terminal = false
			p.Stages[2].Next = "start"
			p.Stages[3].Terminal = false
			p.Stages[3].Next = "start"
		}, "no stage is marked terminal"},
		{"missing template", func(p *Pipeline) { p.Stages[0].Templates = []string{"absent.md"} }, "no file templates/absent.md"},
		{"template typo", func(p *Pipeline) { p.Templates["hello.md"] = "{{ .Item.dued }}" }, "dued"},
		{"template syntax", func(p *Pipeline) { p.Templates["hello.md"] = "{{ .Item.due" }, "has a problem"},
		{"view stage", func(p *Pipeline) { p.Views[0].Where.Stage = StringList{"zzz"} }, `"zzz" in where.stage`},
		{"view field", func(p *Pipeline) { p.Views[0].Where.DueWithin = &DueWithin{Field: "nope", Days: 3} }, `field "nope"`},
		{"view date type", func(p *Pipeline) { p.Views[0].Where.DueWithin = &DueWithin{Field: "notes", Days: 3} }, "needs a date field"},
		{"view window", func(p *Pipeline) { p.Views[2].Windows = nil }, "needs windows"},
		{"view kind", func(p *Pipeline) { p.Views[1].Kind = "pie" }, `kind "pie"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := load(t)
			c.mutate(p)
			problems, _ := p.Check()
			if !strings.Contains(strings.Join(problems, "\n"), c.want) {
				t.Fatalf("want a problem containing %q, got %v", c.want, problems)
			}
		})
	}
}

func TestOneTypoDoesNotFloodTheReport(t *testing.T) {
	p := load(t)
	p.Stages[0].Next = "typo"
	problems, _ := p.Check()
	if len(problems) > 3 {
		t.Fatalf("expected a short report, got %d lines: %v", len(problems), problems)
	}
}

func TestWaitingOn(t *testing.T) {
	cases := map[string]Stage{
		"me":   {Type: TypeDo},
		"them": {Type: TypeWait},
		"":     {Type: TypeWait, Waiting: "none"},
	}
	for want, s := range cases {
		if got := s.WaitingOn(); got != want {
			t.Errorf("%+v: got %q want %q", s, got, want)
		}
	}
	if got := (Stage{Type: TypeWait, Terminal: true}).WaitingOn(); got != "" {
		t.Errorf("terminal stages wait on nobody, got %q", got)
	}
	if got := (Stage{Type: TypeDo, Waiting: "them"}).WaitingOn(); got != "them" {
		t.Errorf("override should win, got %q", got)
	}
}

func TestRenderFillsAndFlagsGaps(t *testing.T) {
	p := load(t)
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	text, missing, err := p.Render("hello.md",
		map[string]string{"name": "Bob Example"}, map[string]string{"due": "2026-10-24"}, "start", now)
	if err != nil || len(missing) != 0 {
		t.Fatalf("err=%v missing=%v", err, missing)
	}
	if want := "Hi Bob, due Saturday, October 24, 2026.\n"; text != want {
		t.Fatalf("got %q want %q", text, want)
	}
	text, missing, err = p.Render("hello.md", map[string]string{}, map[string]string{}, "start", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 || !strings.Contains(text, "[name missing]") || !strings.Contains(text, "[due missing]") {
		t.Fatalf("text=%q missing=%v", text, missing)
	}
}

func TestUnknownKeysInTheFileAreErrors(t *testing.T) {
	_, err := Load("testdata/typo")
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if strings.Contains(err.Error(), "pipeline.Stage") {
		t.Fatalf("error leaks Go type names: %v", err)
	}
	if !strings.Contains(err.Error(), `"labell"`) {
		t.Fatalf("error should name the misspelled key: %v", err)
	}
}
