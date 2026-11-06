// Package collection reads and writes OpenCollection YAML request files. It is
// the only package that knows the file format (see docs/format.md).
//
// Saves edit the yaml.Node tree in place, so key order, comments and unknown
// keys survive. Whole structs are never re-encoded.
package collection

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/abdussalamfaqih/streamx/internal/core"
)

// RootFiles are the collection root file names, in lookup order.
var RootFiles = []string{"opencollection.yml", "opencollection.yaml"}

// File is one request file.
type File struct {
	Path     string
	Root     string // collection root directory (holds opencollection.yml), or the file's dir
	Seq      int
	Req      core.Request
	Warnings []string // features present in the file that streamx ignores
	doc      yaml.Node
}

// Load parses a request file and the collection root config above it.
func Load(p string) (*File, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	f := &File{Path: p}
	if err := yaml.Unmarshal(b, &f.doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	f.Root = FindRoot(filepath.Dir(p))
	if err := f.decode(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return f, nil
}

// FindRoot walks up from dir to the directory holding the collection root file.
// It returns dir itself when there is none, so a lone request file still works.
func FindRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	for d := abs; ; d = filepath.Dir(d) {
		for _, name := range RootFiles {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				return d
			}
		}
		if filepath.Dir(d) == d {
			return abs
		}
	}
}

// IsRequestFile reports whether a YAML file in a collection is a request
// (not the root, a folder, or an environment).
func IsRequestFile(p string) bool {
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	if ext != ".yml" && ext != ".yaml" || base == "folder.yml" || base == "folder.yaml" {
		return false
	}
	for _, r := range RootFiles {
		if base == r {
			return false
		}
	}
	return filepath.Base(filepath.Dir(p)) != "environments"
}

// Set changes the scalar at keys (creating mappings as needed) and re-reads the request.
func (f *File) Set(value string, keys ...string) error {
	if err := f.put(value, keys...); err != nil {
		return err
	}
	return f.decode()
}

// put is Set without re-reading the request, for building a file in steps.
func (f *File) put(value string, keys ...string) error {
	n, err := f.at(yaml.ScalarNode, keys...)
	if err != nil {
		return err
	}
	n.Value, n.Tag = value, "!!str"
	if n.Style == 0 && bytes.ContainsRune([]byte(value), '\n') {
		n.Style = yaml.LiteralStyle
	}
	return nil
}

// at returns the node at keys, creating missing mappings and a final node of
// kind leaf. It checks the whole path before changing anything, so a failed
// call leaves the document untouched.
func (f *File) at(leaf yaml.Kind, keys ...string) (*yaml.Node, error) {
	if len(f.doc.Content) == 0 {
		f.doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if _, err := f.follow(leaf, keys, false); err != nil {
		return nil, err
	}
	return f.follow(leaf, keys, true)
}

// follow walks keys from the document root and checks each node's kind.
// Without create it stops at the first missing key; with create it adds it.
func (f *File) follow(leaf yaml.Kind, keys []string, create bool) (*yaml.Node, error) {
	n := f.doc.Content[0]
	for i, k := range keys {
		want := yaml.MappingNode
		if i == len(keys)-1 {
			want = leaf
		}
		if n.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s is not a mapping", strings.Join(keys[:i], "."))
		}
		child := lookup(n, k)
		if child == nil && !create {
			return nil, nil
		}
		if child == nil {
			child = &yaml.Node{Kind: want}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, child)
		}
		if child.Kind != want {
			return nil, fmt.Errorf("%s is not a %s", strings.Join(keys[:i+1], "."), kindName(want))
		}
		n = child
	}
	return n, nil
}

func kindName(k yaml.Kind) string {
	return map[yaml.Kind]string{yaml.MappingNode: "mapping", yaml.SequenceNode: "list", yaml.ScalarNode: "single value"}[k]
}

// Bytes encodes the file, keeping comments and key order.
func (f *File) Bytes() ([]byte, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(&f.doc); err != nil {
		return nil, err
	}
	return b.Bytes(), e.Close()
}

// Save writes the file back to Path atomically.
func (f *File) Save() error {
	b, err := f.Bytes()
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// --- decoding ---------------------------------------------------------------

type kv struct {
	Name     string
	Value    string
	Disabled bool
}

type assertion struct {
	Expression string
	Operator   string
	Value      string
	Disabled   bool
}

type action struct {
	Type     string
	Phase    string
	Selector struct{ Expression, Method string }
	Variable struct{ Name, Scope string }
	Disabled bool
}

type runtime struct {
	Assertions []assertion
	Actions    []action
	Scripts    []yaml.Node
}

// variants is a value that is either a plain value or a list of
// {title, selected, <key>: value} variants, where the selected one wins.
type variants struct{ node yaml.Node }

func (v *variants) UnmarshalYAML(n *yaml.Node) error { v.node = *n; return nil }

func (v variants) pick(key string, out any) error {
	n := &v.node
	if n.Kind == 0 {
		return nil
	}
	if n.Kind == yaml.SequenceNode {
		if len(n.Content) == 0 {
			return nil
		}
		chosen := n.Content[0]
		for _, c := range n.Content {
			if s := lookup(c, "selected"); s != nil && s.Value == "true" {
				chosen = c
			}
		}
		if n = lookup(chosen, key); n == nil {
			return nil
		}
	}
	return n.Decode(out)
}

type ocFile struct {
	Info struct {
		Name string
		Type string
		Seq  int
	}
	GRPC *struct {
		URL           string
		Method        string
		MethodType    string `yaml:"methodType"`
		ProtoFilePath string `yaml:"protoFilePath"`
		Metadata      []kv
		Message       variants
	} `yaml:"grpc"`
	Websocket *wsDetails `yaml:"websocket"`
	WS        *wsDetails `yaml:"ws"` // Bruno docs spelling
	HTTP      *struct {
		Method  string
		URL     string
		Headers []kv
		Body    variants
		Auth    yaml.Node
		Params  []yaml.Node
	} `yaml:"http"`
	Runtime  runtime
	Settings struct {
		Timeout yaml.Node
	}
	Extensions struct {
		Streamx struct {
			Messages []string
			Stream   struct {
				MaxMessages int    `yaml:"maxMessages"`
				Timeout     string `yaml:"timeout"`
			}
			TLS struct {
				CAFile   string `yaml:"caFile"`
				Insecure bool
			} `yaml:"tls"`
			Assertions []assertion
			Actions    []action
		}
	}
}

type wsDetails struct {
	URL     string
	Headers []kv
	Message variants
}

type rawBody struct{ Type, Data string }

func (f *File) decode() error {
	var oc ocFile
	if err := f.doc.Decode(&oc); err != nil {
		return err
	}
	root, err := LoadRoot(f.Root)
	if err != nil {
		return err
	}
	f.Seq, f.Warnings = oc.Info.Seq, nil
	x := oc.Extensions.Streamx
	r := core.Request{
		Name:        cmp.Or(oc.Info.Name, filepath.Base(f.Path)),
		Type:        oc.Info.Type,
		Messages:    x.Messages,
		MaxMessages: x.Stream.MaxMessages,
		ImportPaths: root.ImportPaths,
		TLS:         core.TLS{CAFile: x.TLS.CAFile, Insecure: x.TLS.Insecure},
	}
	if x.Stream.Timeout != "" {
		if r.Timeout, err = time.ParseDuration(x.Stream.Timeout); err != nil {
			return fmt.Errorf("extensions.streamx.stream.timeout: %w", err)
		}
		r.StreamStop = true
	}
	if err := f.decodeProtocol(&oc, &r); err != nil {
		return err
	}
	r.Assertions = assertions(append(oc.Runtime.Assertions, x.Assertions...))
	if r.Captures, err = captures(append(oc.Runtime.Actions, x.Actions...)); err != nil {
		return err
	}
	if len(oc.Runtime.Scripts) > 0 {
		f.Warnings = append(f.Warnings, "runtime.scripts are not run (streamx has no scripting)")
	}
	f.Req = r
	return nil
}

func (f *File) decodeProtocol(oc *ocFile, r *core.Request) error {
	switch r.Type {
	case "grpc":
		return decodeGRPC(oc, r)
	case "websocket", "ws":
		r.Type = "websocket"
		return decodeWS(oc, r)
	case "http":
		return f.decodeHTTP(oc, r)
	}
	return fmt.Errorf("info.type %q is not supported (grpc, websocket, http)", r.Type)
}

func decodeGRPC(oc *ocFile, r *core.Request) error {
	g := oc.GRPC
	if g == nil {
		return errors.New("info.type is grpc but there is no grpc section")
	}
	r.URL, r.Method, r.MethodType, r.ProtoFile = g.URL, g.Method, g.MethodType, g.ProtoFilePath
	r.Headers = enabled(g.Metadata)
	if err := g.Message.pick("message", &r.Body); err != nil {
		return fmt.Errorf("grpc.message: %w", err)
	}
	return nil
}

func decodeWS(oc *ocFile, r *core.Request) error {
	w := cmp.Or(oc.Websocket, oc.WS)
	if w == nil {
		return errors.New("info.type is websocket but there is no websocket section")
	}
	r.URL, r.Headers = w.URL, enabled(w.Headers)
	var m rawBody
	if err := w.Message.pick("message", &m); err != nil {
		return fmt.Errorf("websocket.message: %w", err)
	}
	r.Body = m.Data
	if t := oc.Settings.Timeout; t.Tag == "!!int" && r.Timeout == 0 { // ms; "inherit" is a string and ignored
		ms, err := strconv.Atoi(t.Value)
		if err != nil {
			return fmt.Errorf("settings.timeout: %w", err)
		}
		r.Timeout, r.StreamStop = time.Duration(ms)*time.Millisecond, true
	}
	return nil
}

func (f *File) decodeHTTP(oc *ocFile, r *core.Request) error {
	h := oc.HTTP
	if h == nil {
		return errors.New("info.type is http but there is no http section")
	}
	r.URL, r.Method, r.Headers = h.URL, h.Method, enabled(h.Headers)
	var b rawBody
	if err := h.Body.pick("body", &b); err != nil {
		return fmt.Errorf("http.body: %w", err)
	}
	if err := setBody(r, b); err != nil {
		return err
	}
	if len(h.Params) > 0 {
		f.Warnings = append(f.Warnings, "http.params are ignored; put query parameters in the url")
	}
	if h.Auth.Kind != 0 && h.Auth.Value != "inherit" && h.Auth.Value != "none" {
		f.Warnings = append(f.Warnings, "http.auth is ignored; set an Authorization header instead")
	}
	return nil
}

// setBody sets an HTTP body and, unless one is set, its Content-Type.
func setBody(r *core.Request, b rawBody) error {
	ctype, ok := map[string]string{"": "", "json": "application/json", "text": "text/plain", "xml": "application/xml"}[b.Type]
	if !ok {
		return fmt.Errorf("http.body type %q is not supported yet (json, text, xml only)", b.Type)
	}
	r.Body = b.Data
	if ctype != "" && !hasHeader(r.Headers, "Content-Type") {
		r.Headers = append(r.Headers, core.KV{Name: "Content-Type", Value: ctype})
	}
	return nil
}

func assertions(in []assertion) []core.Assertion {
	var out []core.Assertion
	for _, a := range in {
		if !a.Disabled {
			out = append(out, core.Assertion{Expression: a.Expression, Operator: a.Operator, Value: a.Value})
		}
	}
	return out
}

// captures turns after-response set-variable actions into captures.
func captures(in []action) ([]core.Capture, error) {
	var out []core.Capture
	for _, a := range in {
		if a.Disabled || a.Phase == "before-request" {
			continue
		}
		if a.Type != "set-variable" || (a.Selector.Method != "" && a.Selector.Method != "jsonq") {
			return nil, fmt.Errorf("runtime action %q with selector method %q is not supported", a.Type, a.Selector.Method)
		}
		out = append(out, core.Capture{Name: a.Variable.Name, Expression: a.Selector.Expression})
	}
	return out, nil
}

func hasHeader(hs []core.KV, name string) bool {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return true
		}
	}
	return false
}

func enabled(in []kv) []core.KV {
	var out []core.KV
	for _, h := range in {
		if !h.Disabled {
			out = append(out, core.KV{Name: h.Name, Value: h.Value})
		}
	}
	return out
}

// --- collection root ----------------------------------------------------------

type ClientCert struct {
	Domain              string
	Type                string
	CertificateFilePath string `yaml:"certificateFilePath"`
	PrivateKeyFilePath  string `yaml:"privateKeyFilePath"`
	Disabled            bool
}

// Root is the parts of opencollection.yml that requests depend on.
type Root struct {
	Dir         string
	ImportPaths []string
	Certs       []ClientCert
	// Environments holds config.environments entries undecoded; package env reads them.
	Environments []yaml.Node
}

// LoadRoot reads the collection root file in dir. A missing file gives an empty Root.
func LoadRoot(dir string) (*Root, error) {
	r := &Root{Dir: dir}
	for _, name := range RootFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var oc struct {
			Config struct {
				Protobuf struct {
					ImportPaths []struct {
						Path     string
						Disabled bool
					} `yaml:"importPaths"`
				}
				ClientCertificates []ClientCert `yaml:"clientCertificates"`
				Environments       []yaml.Node
			}
		}
		if err := yaml.Unmarshal(b, &oc); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, ip := range oc.Config.Protobuf.ImportPaths {
			if !ip.Disabled {
				r.ImportPaths = append(r.ImportPaths, ip.Path)
			}
		}
		r.Certs, r.Environments = oc.Config.ClientCertificates, oc.Config.Environments
		break
	}
	return r, nil
}

// ApplyCert sets the client cert of the first enabled cert whose domain
// pattern matches the request host, unless the request already has one.
func ApplyCert(req core.Request, certs []ClientCert) (core.Request, error) {
	host, hostOnly, ok := certHost(req.URL)
	if req.TLS.CertFile != "" || !ok {
		return req, nil
	}
	for _, c := range certs {
		m1, _ := path.Match(c.Domain, host)
		m2, _ := path.Match(c.Domain, hostOnly)
		if c.Disabled || !m1 && !m2 {
			continue
		}
		if c.Type != "" && c.Type != "pem" {
			return req, fmt.Errorf("client certificate for %s: type %q is not supported yet (pem only)", c.Domain, c.Type)
		}
		req.TLS.CertFile, req.TLS.KeyFile = c.CertificateFilePath, c.PrivateKeyFilePath
		return req, nil
	}
	return req, nil
}

// certHost returns the URL's host with and without port; ok is false for
// plaintext schemes, where a client certificate can't apply.
func certHost(raw string) (host, hostOnly string, ok bool) {
	host = raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		switch u.Scheme {
		case "grpc", "http", "ws":
			return "", "", false
		}
		host = u.Host
	}
	hostOnly = host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}
	return host, hostOnly, true
}

// ResolvePaths makes file paths in req absolute, relative to the collection root.
// Call it after interpolation so {{vars}} in paths are already expanded.
func ResolvePaths(req core.Request, root string) core.Request {
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}
	req.ProtoFile = abs(req.ProtoFile)
	ips := make([]string, len(req.ImportPaths))
	for i, p := range req.ImportPaths {
		ips[i] = abs(p)
	}
	req.ImportPaths = ips
	req.TLS.CAFile, req.TLS.CertFile, req.TLS.KeyFile = abs(req.TLS.CAFile), abs(req.TLS.CertFile), abs(req.TLS.KeyFile)
	return req
}

// --- editing helpers for the TUI -----------------------------------------------

// New builds an unsaved request file in the collection at root.
func New(root string, r core.Request) (*File, error) {
	f := &File{Root: root, Req: core.Request{Type: r.Type}} // Type picks BodyKeys before the first decode
	sec := r.Type
	sets := [][]string{{r.Name, "info", "name"}, {r.Type, "info", "type"}, {r.URL, sec, "url"}}
	if r.Type == "grpc" {
		sets = append(sets, []string{r.Method, "grpc", "method"}, []string{r.MethodType, "grpc", "methodType"}, []string{r.ProtoFile, "grpc", "protoFilePath"})
	}
	if r.Type == "http" {
		sets = append(sets, []string{r.Method, "http", "method"})
	}
	if r.Body != "" {
		sets = append(sets, append([]string{r.Body}, f.BodyKeys()...))
	}
	for _, s := range sets {
		if s[0] != "" {
			if err := f.put(s[0], s[1:]...); err != nil {
				return nil, err
			}
		}
	}
	return f, f.decode()
}

// Section is the top-level key holding the request details.
func (f *File) Section() string {
	if f.Req.Type == "websocket" && len(f.doc.Content) > 0 && lookup(f.doc.Content[0], "ws") != nil {
		return "ws"
	}
	return f.Req.Type
}

// BodyKeys is where the single message or body lives for this request type.
func (f *File) BodyKeys() []string {
	switch f.Req.Type {
	case "http":
		return []string{"http", "body", "data"}
	case "websocket":
		return []string{f.Section(), "message", "data"}
	}
	return []string{"grpc", "message"}
}

// SetHeader sets (or, with an empty value, removes) a gRPC metadata entry or
// HTTP/WebSocket header, matching names case-insensitively.
func (f *File) SetHeader(name, value string) error {
	key := "headers"
	if f.Req.Type == "grpc" {
		key = "metadata"
	}
	seq, err := f.at(yaml.SequenceNode, f.Section(), key)
	if err != nil {
		return err
	}
	for i, item := range seq.Content {
		if n := lookup(item, "name"); n != nil && strings.EqualFold(n.Value, name) {
			if value == "" {
				seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
			} else if v := lookup(item, "value"); v != nil {
				v.Value, v.Tag, v.Style = value, "!!str", 0
			} else {
				item.Content = append(item.Content, scalar("value"), scalar(value))
			}
			return f.decode()
		}
	}
	if value != "" {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			scalar("name"), scalar(name), scalar("value"), scalar(value),
		}})
	}
	return f.decode()
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

// SaveAs writes a new file at p and makes it the file's path. It never
// overwrites an existing file.
func (f *File) SaveAs(p string) error {
	b, err := f.Bytes()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := out.Write(b); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	f.Path = p
	return nil
}

// InitRoot makes dir a collection if it is not inside one: it writes
// opencollection.yml and, if missing, a .gitignore that keeps secrets (.env)
// and transcripts out of git. It reports whether it created the collection.
func InitRoot(dir string) (bool, error) {
	for _, name := range RootFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return false, nil
		}
	}
	name, _ := filepath.Abs(dir)
	root := fmt.Sprintf("opencollection: 1.0.0\ninfo:\n  name: %q\n", filepath.Base(name))
	if err := os.WriteFile(filepath.Join(dir, RootFiles[0]), []byte(root), 0o644); err != nil {
		return false, err
	}
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(gi, []byte("# secret values (environments' dotEnvFilePath)\n.env\n*.env\n# stream transcripts\n*.jsonl\n"), 0o644); err != nil {
			return true, err
		}
	}
	return true, nil
}
