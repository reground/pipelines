// Package pipeline defines what a pipeline is (stages, fields, templates,
// views), loads it from a directory of plain text files, and checks it.
// A pipeline is configuration, not data: it lives in git, and the engine
// only ever reads it.
package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Stage types. Only "wait" changes behavior today (the ball is with them);
// the rest are labels for what the person is doing at that stage.
const (
	TypeDo    = "do"    // something the user handles, then confirms
	TypeDraft = "draft" // show a template filled in for this contact
	TypeSend  = "send"  // like draft, for the step where it goes out
	TypeWait  = "wait"  // the ball is with someone else
)

// Field types.
const (
	FieldText     = "text"
	FieldLongText = "longtext"
	FieldNumber   = "number"
	FieldDate     = "date"
	FieldURL      = "url"
)

// View kinds.
const (
	KindList   = "list"   // the items matching a filter
	KindCounts = "counts" // how many items sit in each stage
	KindEvents = "events" // how many items entered a stage within a window
	KindCohort = "cohort" // of items that entered a stage, how many reached an outcome
)

// StringList accepts either a single name or a list of names in YAML.
type StringList []string

func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var v string
		if err := n.Decode(&v); err != nil {
			return err
		}
		*s = StringList{v}
	case yaml.SequenceNode:
		var v []string
		if err := n.Decode(&v); err != nil {
			return err
		}
		*s = v
	default:
		return fmt.Errorf("line %d: expected a single name or a list of names", n.Line)
	}
	return nil
}

type Pipeline struct {
	ID     string  `yaml:"id"`
	Label  string  `yaml:"label"`
	Help   string  `yaml:"help"`
	Fields []Field `yaml:"fields"`
	Stages []Stage `yaml:"stages"`
	Views  []View  `yaml:"views"`

	// Templates maps a template file name to its body. Filled by Load.
	Templates map[string]string `yaml:"-"`
}

type Field struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
	Type  string `yaml:"type"`
}

type Stage struct {
	ID        string     `yaml:"id"`
	Label     string     `yaml:"label"`
	Type      string     `yaml:"type"`
	Help      string     `yaml:"help"`  // what to do at this stage
	Next      string     `yaml:"next"`  // the default step forward
	Exits     StringList `yaml:"exits"` // other places an item can go from here
	Terminal  bool       `yaml:"terminal"`
	Waiting   string     `yaml:"waiting_on"` // me | them | none; overrides the type's default
	Templates StringList `yaml:"templates"`  // template files shown at this stage
	Checklist StringList `yaml:"checklist"`  // reminders shown at this stage
}

// WaitingOn derives who the ball is with from the stage, so nobody has to
// keep a "waiting on" field up to date by hand. Empty means nobody: the
// item is finished, or is just waiting for a date to arrive.
func (s Stage) WaitingOn() string {
	switch {
	case s.Terminal:
		return ""
	case s.Waiting == "none":
		return ""
	case s.Waiting == "me" || s.Waiting == "them":
		return s.Waiting
	case s.Type == TypeWait:
		return "them"
	default:
		return "me"
	}
}

type View struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
	Kind  string `yaml:"kind"`

	// list
	Where Where  `yaml:"where"`
	Sort  string `yaml:"sort"`
	Desc  bool   `yaml:"desc"`

	// events: items whose first entry into one of these stages falls in
	// each window (days). cohort uses Event as the cohort definition.
	Event   *EnteredSpec `yaml:"event"`
	Windows []int        `yaml:"windows"`

	// cohort: of the items in the cohort, how many ever reached Outcome.
	// The cohort window ends EndingDaysAgo days ago, so it has had time to
	// answer.
	Outcome       *EnteredSpec `yaml:"outcome"`
	WindowDays    int          `yaml:"window_days"`
	EndingDaysAgo int          `yaml:"ending_days_ago"`
}

type EnteredSpec struct {
	Entered StringList `yaml:"entered"`
}

// Where selects items. All conditions given must hold.
type Where struct {
	Stage           StringList `yaml:"stage"`
	StageNot        StringList `yaml:"stage_not"`
	WaitingOn       string     `yaml:"waiting_on"` // me | them
	Active          *bool      `yaml:"active"`     // not in a terminal stage
	DueWithin       *DueWithin `yaml:"due_within"`
	DaysInStageOver int        `yaml:"days_in_stage_over"`
}

// DueWithin matches items whose date field is on or before N days from
// today. Overdue items match too, which is what you want for "at risk".
type DueWithin struct {
	Field string `yaml:"field"`
	Days  int    `yaml:"days"`
}

// Stage returns the stage with the given id.
func (p *Pipeline) Stage(id string) (Stage, bool) {
	for _, s := range p.Stages {
		if s.ID == id {
			return s, true
		}
	}
	return Stage{}, false
}

// Field returns the field with the given id.
func (p *Pipeline) Field(id string) (Field, bool) {
	for _, f := range p.Fields {
		if f.ID == id {
			return f, true
		}
	}
	return Field{}, false
}

// View returns the view with the given id.
func (p *Pipeline) View(id string) (View, bool) {
	for _, v := range p.Views {
		if v.ID == id {
			return v, true
		}
	}
	return View{}, false
}

// FirstStage is where new items start.
func (p *Pipeline) FirstStage() Stage { return p.Stages[0] }

// StagesWaitingOn returns the ids of stages where the ball is with who.
func (p *Pipeline) StagesWaitingOn(who string) []string {
	var out []string
	for _, s := range p.Stages {
		if s.WaitingOn() == who {
			out = append(out, s.ID)
		}
	}
	return out
}

// TerminalStages returns the ids of finished stages.
func (p *Pipeline) TerminalStages() []string {
	var out []string
	for _, s := range p.Stages {
		if s.Terminal {
			out = append(out, s.ID)
		}
	}
	return out
}

// Load reads one pipeline directory: pipeline.yml plus templates/*.
// Unknown keys are an error, so a typo in the file is caught instead of
// silently ignored.
func Load(dir string) (*Pipeline, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "pipeline.yml"))
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var p Pipeline
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s: %s", filepath.Join(filepath.Base(dir), "pipeline.yml"), friendlyYAML(err))
	}
	p.Templates = map[string]string{}
	entries, err := os.ReadDir(filepath.Join(dir, "templates"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "templates", e.Name()))
		if err != nil {
			return nil, err
		}
		p.Templates[e.Name()] = string(b)
	}
	return &p, nil
}

// LoadAll loads every subdirectory of root that contains a pipeline.yml,
// ordered by pipeline id.
func LoadAll(root string) ([]*Pipeline, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []*Pipeline
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "pipeline.yml")); err != nil {
			continue
		}
		p, err := Load(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

var unknownKeyRe = regexp.MustCompile(`line (\d+): field (\S+) not found in type pipeline\.(\w+)`)

var yamlThings = map[string]string{
	"Pipeline": "pipeline", "Field": "field", "Stage": "stage", "View": "view",
	"Where": "where clause", "DueWithin": "due_within", "EnteredSpec": "event or outcome",
}

// friendlyYAML rewrites the parser's messages for people who do not know
// what the engine's types are called.
func friendlyYAML(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: ")
	msg = strings.ReplaceAll(msg, "yaml: ", "")
	return unknownKeyRe.ReplaceAllStringFunc(msg, func(m string) string {
		sub := unknownKeyRe.FindStringSubmatch(m)
		thing := yamlThings[sub[3]]
		if thing == "" {
			thing = sub[3]
		}
		return fmt.Sprintf("line %s: %q is not a setting a %s can have (check the spelling)", sub[1], sub[2], thing)
	})
}

// LoadDirs loads every pipeline directory under root, one by one, so a
// problem in one does not hide the others. The result maps the directory
// name to its pipeline or to the error that stopped it loading.
func LoadDirs(root string) (loaded map[string]*Pipeline, errs map[string]error, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	loaded, errs = map[string]*Pipeline{}, map[string]error{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, statErr := os.Stat(filepath.Join(dir, "pipeline.yml")); statErr != nil {
			continue
		}
		p, loadErr := Load(dir)
		if loadErr != nil {
			errs[e.Name()] = loadErr
			continue
		}
		loaded[e.Name()] = p
	}
	return loaded, errs, nil
}

// Outline is a plain-text description of the pipeline, in stage order.
func (p *Pipeline) Outline() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", p.Label, p.ID)
	for i, s := range p.Stages {
		fmt.Fprintf(&b, "%d. %s [%s]", i+1, s.Label, s.Type)
		if s.Terminal {
			b.WriteString(" - finished")
		} else if w := s.WaitingOn(); w == "them" {
			b.WriteString(" - waiting on them")
		}
		b.WriteString("\n")
		if s.Next != "" {
			if n, ok := p.Stage(s.Next); ok {
				fmt.Fprintf(&b, "   next: %s\n", n.Label)
			}
		}
		for _, x := range s.Exits {
			if n, ok := p.Stage(x); ok {
				fmt.Fprintf(&b, "   or: %s\n", n.Label)
			}
		}
	}
	return b.String()
}
