// Package env loads OpenCollection environments, expands {{var}} placeholders,
// and redacts secret values from everything streamx prints or saves.
//
// Secrets: variables marked `secret: true` (values from the environment's
// dotEnvFilePath file, then OS env vars), {{process.env.NAME}} references, and
// captured values. Files only ever hold the raw placeholders.
package env

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/abdussalamfaqih/streamx/internal/collection"
	"github.com/abdussalamfaqih/streamx/internal/core"
)

const mask = "****"

// Env is the variable set for one run. It is safe for concurrent use.
type Env struct {
	mu     sync.RWMutex
	vars   map[string]string
	secret map[string]bool // values (not names) to redact
	dotenv map[string]string
	Certs  []collection.ClientCert
}

func New() *Env {
	return &Env{vars: map[string]string{}, secret: map[string]bool{}, dotenv: map[string]string{}}
}

type variable struct {
	Name     string
	Value    yaml.Node
	Secret   bool
	Disabled bool
}

type environment struct {
	Name               string
	Variables          []variable
	Extends            string
	DotEnvFilePath     string                  `yaml:"dotEnvFilePath"`
	ClientCertificates []collection.ClientCert `yaml:"clientCertificates"`
	ExternalSecrets    yaml.Node               `yaml:"externalSecrets"`
}

// Load reads environment name from root/environments/<name>.yml or the root
// file's config.environments. An empty name gives an empty Env.
func Load(root, name string) (*Env, error) {
	e := New()
	if name == "" {
		return e, nil
	}
	return e, e.load(root, name, 0)
}

func (e *Env) load(root, name string, depth int) error {
	if depth > 10 {
		return fmt.Errorf("environment %q: extends chain too deep", name)
	}
	envDef, err := find(root, name)
	if err != nil {
		return err
	}
	if envDef.Extends != "" {
		if err := e.load(root, envDef.Extends, depth+1); err != nil {
			return err
		}
	}
	if envDef.ExternalSecrets.Kind != 0 {
		return fmt.Errorf("environment %q: externalSecrets are not supported yet", name)
	}
	if err := e.loadDotEnv(root, envDef.DotEnvFilePath); err != nil {
		return fmt.Errorf("environment %q: %w", name, err)
	}
	for _, v := range envDef.Variables {
		e.define(v)
	}
	e.Certs = append(envDef.ClientCertificates, e.Certs...) // child certs win
	return nil
}

// loadDotEnv reads the env's .env file, if it has one. A missing file is
// fine: CI sets secrets through OS env vars instead.
func (e *Env) loadDotEnv(root, p string) error {
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	if err := readDotEnv(p, e.dotenv); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// define sets one variable. A secret takes its value from .env, then the OS
// environment; an unset secret stays undefined and fails only if referenced.
func (e *Env) define(v variable) {
	switch {
	case v.Disabled || v.Name == "":
	case !v.Secret:
		e.vars[v.Name] = value(&v.Value)
	default:
		val, ok := e.dotenv[v.Name]
		if !ok {
			val, ok = os.LookupEnv(v.Name)
		}
		if ok {
			e.Set(v.Name, val, true)
		}
	}
}

func find(root, name string) (*environment, error) {
	var envDef environment
	for _, ext := range []string{".yml", ".yaml"} {
		b, err := os.ReadFile(filepath.Join(root, "environments", name+ext))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, &envDef); err != nil {
			return nil, fmt.Errorf("environment %q: %w", name, err)
		}
		return &envDef, nil
	}
	r, err := collection.LoadRoot(root)
	if err != nil {
		return nil, err
	}
	for i := range r.Environments {
		if err := r.Environments[i].Decode(&envDef); err != nil {
			return nil, err
		}
		if envDef.Name == name {
			return &envDef, nil
		}
	}
	return nil, fmt.Errorf("environment %q not found in %s", name, filepath.Join(root, "environments"))
}

// value reads a variable value: a scalar, a typed {type, data}, or variants
// [{title, selected, value}] where the selected one wins.
func value(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.MappingNode:
		if d := field(n, "data"); d != nil {
			return d.Value
		}
	case yaml.SequenceNode:
		if v := selected(n); v != nil {
			return value(v)
		}
	}
	return ""
}

// selected returns the value of the selected variant, else of the first one.
func selected(n *yaml.Node) *yaml.Node {
	var chosen *yaml.Node
	for _, c := range n.Content {
		if sel := field(c, "selected"); chosen == nil || (sel != nil && sel.Value == "true") {
			chosen = field(c, "value")
		}
	}
	return chosen
}

// field returns the value of key in mapping n, or nil.
func field(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// readDotEnv parses KEY=VALUE lines; # comments, optional "export " and quotes.
func readDotEnv(p string, into map[string]string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if k, v, ok := dotEnvLine(s.Text()); ok {
			into[k] = v
		}
	}
	return s.Err()
}

func dotEnvLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(k), v, ok
}

// Set defines a variable. Secret values are redacted from all output.
func (e *Env) Set(name, val string, secret bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[name] = val
	if secret && val != "" {
		e.secret[val] = true
	}
}

// UndefinedError is returned when a placeholder names no known variable.
type UndefinedError struct{ Name string }

func (e *UndefinedError) Error() string { return fmt.Sprintf("undefined variable %q", e.Name) }

// minCapturedSecret is the length from which a captured value counts as a
// secret. Shorter ones (ids, flags) would mask every matching substring.
const minCapturedSecret = 8

// Capture stores a value captured from a response. Values of 8+ characters
// (tokens, keys) are masked like secrets; shorter ones are shown.
func (e *Env) Capture(name, val string) {
	e.Set(name, val, len(val) >= minCapturedSecret)
}

var placeholder = regexp.MustCompile(`\{\{\s*([^{}\s]+)\s*\}\}`)

// Expand replaces {{name}} and {{process.env.NAME}}. Values may themselves
// hold placeholders. An undefined variable is an error.
func (e *Env) Expand(s string) (string, error) {
	for depth := 0; strings.Contains(s, "{{"); depth++ {
		if depth == 10 {
			return "", fmt.Errorf("variables nest more than 10 levels deep in %q", s)
		}
		var missing []string
		s = placeholder.ReplaceAllStringFunc(s, func(m string) string {
			name := placeholder.FindStringSubmatch(m)[1]
			if v, ok := e.lookup(name); ok {
				return v
			}
			missing = append(missing, name)
			return m
		})
		if len(missing) > 0 {
			return "", &UndefinedError{Name: missing[0]}
		}
		if !placeholder.MatchString(s) {
			break // a literal "{{" that is not a placeholder
		}
	}
	return s, nil
}

func (e *Env) lookup(name string) (string, bool) {
	if osName, ok := strings.CutPrefix(name, "process.env."); ok {
		e.mu.RLock()
		v, found := e.dotenv[osName]
		e.mu.RUnlock()
		if !found {
			v, found = os.LookupEnv(osName)
		}
		if found {
			e.mu.Lock()
			if v != "" {
				e.secret[v] = true
			}
			e.mu.Unlock()
		}
		return v, found
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	v, ok := e.vars[name]
	return v, ok
}

// ExpandRequest returns a copy of r with every placeholder expanded.
func (e *Env) ExpandRequest(r core.Request) (core.Request, error) {
	var err error
	x := func(s *string) {
		if err == nil {
			*s, err = e.Expand(*s)
		}
	}
	for _, p := range []*string{&r.URL, &r.Method, &r.MethodType, &r.ProtoFile, &r.Body, &r.TLS.CAFile, &r.TLS.CertFile, &r.TLS.KeyFile} {
		x(p)
	}
	r.ImportPaths = append([]string(nil), r.ImportPaths...)
	for i := range r.ImportPaths {
		x(&r.ImportPaths[i])
	}
	r.Messages = append([]string(nil), r.Messages...)
	for i := range r.Messages {
		x(&r.Messages[i])
	}
	r.Headers = append([]core.KV(nil), r.Headers...)
	for i := range r.Headers {
		x(&r.Headers[i].Name)
		x(&r.Headers[i].Value)
	}
	r.Assertions = append([]core.Assertion(nil), r.Assertions...)
	for i := range r.Assertions {
		x(&r.Assertions[i].Value)
	}
	return r, err
}

// Redact masks every known secret value in s, including its JSON-escaped form.
func (e *Env) Redact(s string) string {
	e.mu.RLock()
	vals := make([]string, 0, len(e.secret))
	for v := range e.secret {
		vals = append(vals, v)
	}
	e.mu.RUnlock()
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) }) // longest first
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, mask)
		if q, _ := json.Marshal(v); string(q[1:len(q)-1]) != v {
			s = strings.ReplaceAll(s, string(q[1:len(q)-1]), mask)
		}
	}
	return s
}

// RedactEvent masks secrets in every text field of ev.
func (e *Env) RedactEvent(ev core.Event) core.Event {
	if len(ev.Data) > 0 {
		ev.Data = json.RawMessage(e.Redact(string(ev.Data)))
	}
	if len(ev.Meta) > 0 {
		m := make(map[string]string, len(ev.Meta))
		for k, v := range ev.Meta {
			m[k] = e.Redact(v)
		}
		ev.Meta = m
	}
	ev.Err, ev.Text = e.Redact(ev.Err), e.Redact(ev.Text)
	return ev
}

// RedactEvents wraps an event stream so no sink can see an unredacted event.
// The caller must drain the returned channel.
func (e *Env) RedactEvents(in <-chan core.Event) <-chan core.Event {
	out := make(chan core.Event, cap(in))
	go func() {
		defer close(out)
		for ev := range in {
			out <- e.RedactEvent(ev)
		}
	}()
	return out
}
