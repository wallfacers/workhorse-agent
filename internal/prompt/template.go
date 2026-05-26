package prompt

import (
	"bytes"
	"text/template"
)

// Template wraps a compiled text/template with a human-readable name for
// error messages and debugging.
type Template struct {
	name string
	tmpl *template.Template
}

// MustParse compiles a template at init time. It panics on parse errors so
// bugs are caught before the binary starts serving requests.
func MustParse(name, body string) *Template {
	t := template.Must(template.New(name).Parse(body))
	return &Template{name: name, tmpl: t}
}

// Execute renders the template with the given data map.
//
// Security: data values should be basic types (string, int, bool) or
// []map[string]string. Do not pass structs, funcs, or chans.
// text/template treats data values as raw strings (no secondary parsing),
// providing native SSTI immunity.
func (t *Template) Execute(data map[string]any) (string, error) {
	var buf bytes.Buffer
	if err := t.tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
