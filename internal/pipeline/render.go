package pipeline

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"
)

// ContactKeys are the contact details a template may use, as
// {{ .Contact.name }} and so on.
var ContactKeys = []string{"name", "email", "website", "notes"}

// templateData is what a template sees. Item holds the pipeline's own
// fields by id, e.g. {{ .Item.swap_date }}.
type templateData struct {
	Contact  map[string]string
	Item     map[string]string
	Stage    string
	Pipeline string
	Today    string
}

var missingRe = regexp.MustCompile(`\[[a-z_0-9]+ missing\]`)

func funcs() template.FuncMap {
	return template.FuncMap{
		// first returns the first word of a name: {{ first .Contact.name }}
		"first": func(s string) string {
			if strings.HasPrefix(s, "[") {
				return s
			}
			if f := strings.Fields(s); len(f) > 0 {
				return f[0]
			}
			return s
		},
		// date writes an ISO date out in full: {{ date .Item.swap_date }}
		"date": func(s string) string {
			t, err := time.Parse("2006-01-02", s)
			if err != nil {
				return s
			}
			return t.Format("Monday, January 2, 2006")
		},
	}
}

// Render fills a template in for one contact and item. Anything the
// template asks for that is empty is shown as "[field missing]" in the
// text and listed in missing, so a half-known contact still gets a useful
// draft and it is obvious what to fill in.
func (p *Pipeline) Render(name string, contact, item map[string]string, stage string, today time.Time) (text string, missing []string, err error) {
	body, ok := p.Templates[name]
	if !ok {
		return "", nil, fmt.Errorf("template %q not found", name)
	}
	t, err := template.New(name).Funcs(funcs()).Option("missingkey=error").Parse(body)
	if err != nil {
		return "", nil, err
	}
	data := templateData{
		Contact:  map[string]string{},
		Item:     map[string]string{},
		Stage:    stage,
		Pipeline: p.ID,
		Today:    today.Format("2006-01-02"),
	}
	for _, k := range ContactKeys {
		data.Contact[k] = orMissing(contact[k], k)
	}
	for _, f := range p.Fields {
		data.Item[f.ID] = orMissing(item[f.ID], f.ID)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", nil, err
	}
	text = buf.String()
	seen := map[string]bool{}
	for _, m := range missingRe.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			missing = append(missing, strings.TrimSuffix(strings.TrimPrefix(m, "["), " missing]"))
		}
	}
	return text, missing, nil
}

func orMissing(v, key string) string {
	if strings.TrimSpace(v) == "" {
		return "[" + key + " missing]"
	}
	return v
}

// checkTemplate parses and executes a template against dummy data, so a
// misspelled field name is caught when the pipeline is checked rather than
// when the template is first used.
func (p *Pipeline) checkTemplate(name string) error {
	body := p.Templates[name]
	t, err := template.New(name).Funcs(funcs()).Option("missingkey=error").Parse(body)
	if err != nil {
		return err
	}
	data := templateData{
		Contact: map[string]string{}, Item: map[string]string{},
		Stage: "x", Pipeline: p.ID, Today: "2006-01-02",
	}
	for _, k := range ContactKeys {
		data.Contact[k] = "x"
	}
	for _, f := range p.Fields {
		data.Item[f.ID] = "x"
	}
	return t.Execute(&bytes.Buffer{}, data)
}
