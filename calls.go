package java

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// CallMethod is one method in the call graph. Overloads share an entry, because a call is matched by name.
type CallMethod struct {
	Class     string   `json:"class"`
	Name      string   `json:"name"`
	File      string   `json:"file"`
	Line      int      `json:"line"`
	EndLine   int      `json:"-"`
	Signature string   `json:"signature"`
	Test      bool     `json:"-"`
	Routes    []string `json:"routes,omitempty"` // REST routes this method handles
	key       string
}

// MethodCall is one method calling another. Confirmed means the receiver's type was known;
// otherwise the call was matched by name only.
type MethodCall struct {
	From      *CallMethod `json:"from"`
	To        *CallMethod `json:"to"`
	Confirmed bool        `json:"confirmed"`
}

// CallGraph records who calls what, from syntax and declared types, built the way the first Codist
// built it. It cannot see reflection, events or configuration.
type CallGraph struct {
	methods map[string]*CallMethod
	callers map[string]map[string]bool // target -> caller -> the receiver's type was known
	related map[string][]string        // class -> classes above and below it in the hierarchy
}

func moduleOfFile(path string) string {
	if i := strings.Index(path, "/src/"); i > 0 {
		return path[:i]
	}
	return ""
}

func isTestFile(path string) bool {
	base := filepath.Base(path)
	return strings.Contains("/"+path, "/src/test/") || strings.HasSuffix(base, "Test.java") || strings.HasSuffix(base, "Tests.java")
}

func methodKey(module, class, name string) string { return module + "|" + class + "#" + name }
func classKey(module, class string) string        { return module + "|" + class }

func findAll(n *ts.Node, typ string) []*ts.Node {
	var out []*ts.Node
	var walk func(x *ts.Node)
	walk = func(x *ts.Node) {
		if x.Type(lang) == typ {
			out = append(out, x)
		}
		for i := 0; i < x.NamedChildCount(); i++ {
			walk(x.NamedChild(i))
		}
	}
	if n != nil {
		walk(n)
	}
	return out
}

func childOfType(n *ts.Node, typ string) *ts.Node {
	for _, c := range named(n) {
		if c.Type(lang) == typ {
			return c
		}
	}
	return nil
}

// declaredVariables is the variables a method can see beyond its class's fields, with their types.
func declaredVariables(f *File, m *ts.Node) map[string]string {
	vars := map[string]string{}
	for _, p := range findAll(m, "formal_parameter") {
		vars[f.text(field(p, "name"))] = f.baseType(field(p, "type"))
	}
	for _, d := range findAll(m, "local_variable_declaration") {
		t := f.baseType(field(d, "type"))
		for _, v := range findAll(d, "variable_declarator") {
			kind := t
			if value := field(v, "value"); kind == "" && value != nil && value.Type(lang) == "object_creation_expression" {
				kind = f.baseType(field(value, "type"))
			}
			vars[f.text(field(v, "name"))] = kind
		}
	}
	for _, d := range findAll(m, "enhanced_for_statement") {
		vars[f.text(field(d, "name"))] = f.baseType(field(d, "type"))
	}
	return vars
}

type classDecl struct {
	f       *File
	node    *ts.Node
	name    string
	module  string
	path    string
	fields  map[string]string
	methods []*ts.Node
	supers  []string
}

// BuildCallGraph reads every source file and links method calls to the methods they reach. edges
// and types supply the Feign links, so a client method counts as a caller of the handler it reaches.
func BuildCallGraph(sources map[string][]byte, types []*Type) *CallGraph {
	cg := &CallGraph{methods: map[string]*CallMethod{}, callers: map[string]map[string]bool{}, related: map[string][]string{}}
	var decls []*classDecl
	byName := map[string][]*classDecl{}
	byMethodName := map[string][]string{}
	paths := make([]string, 0, len(sources))
	for p := range sources {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		f := Parse(path, sources[path])
		if f.Root == nil {
			continue
		}
		var found []*ts.Node
		var walk func(x *ts.Node)
		walk = func(x *ts.Node) {
			if _, ok := typeKinds[x.Type(lang)]; ok {
				found = append(found, x)
			}
			for i := 0; i < x.NamedChildCount(); i++ {
				walk(x.NamedChild(i))
			}
		}
		walk(f.Root)
		for _, n := range found {
			d := &classDecl{f: f, node: n, name: f.text(field(n, "name")), module: moduleOfFile(path), path: path, fields: map[string]string{}}
			for _, t := range typeList(field(n, "superclass")) {
				d.supers = append(d.supers, f.baseType(t))
			}
			for _, t := range typeList(childOfType(n, "extends_interfaces")) {
				d.supers = append(d.supers, f.baseType(t))
			}
			for _, t := range typeList(field(n, "interfaces")) {
				d.supers = append(d.supers, f.baseType(t))
			}
			for _, c := range members(field(n, "body")) {
				switch c.Type(lang) {
				case "field_declaration":
					t := f.baseType(field(c, "type"))
					for _, v := range findAll(c, "variable_declarator") {
						d.fields[f.text(field(v, "name"))] = t
					}
				case "method_declaration":
					d.methods = append(d.methods, c)
				}
			}
			if params := field(n, "parameters"); params != nil { // record components
				for _, p := range named(params) {
					d.fields[f.text(field(p, "name"))] = f.baseType(field(p, "type"))
				}
			}
			decls = append(decls, d)
			byName[d.name] = append(byName[d.name], d)
			for _, m := range d.methods {
				name := f.text(field(m, "name"))
				key := methodKey(d.module, d.name, name)
				if cg.methods[key] == nil {
					cg.methods[key] = &CallMethod{Class: d.name, Name: name, File: path, Line: line(m), EndLine: endLine(m),
						Signature: f.signature(m), Test: isTestFile(path), key: key}
					byMethodName[name] = append(byMethodName[name], key)
				} else if end := endLine(m); end > cg.methods[key].EndLine {
					cg.methods[key].EndLine = end // an overload further down the file
				}
			}
		}
	}

	// A type name means the one in the same module; otherwise any module's, less certainly if several.
	resolve := func(module, name string) ([]*classDecl, bool) {
		for _, d := range byName[name] {
			if d.module == module {
				return []*classDecl{d}, true
			}
		}
		return byName[name], len(byName[name]) == 1
	}
	for _, d := range decls {
		for _, s := range d.supers {
			found, _ := resolve(d.module, s)
			for _, up := range found {
				a, b := classKey(d.module, d.name), classKey(up.module, up.name)
				cg.related[a] = append(cg.related[a], b)
				cg.related[b] = append(cg.related[b], a)
			}
		}
	}
	link := func(target, caller string, sure bool) {
		if target == caller {
			return
		}
		if cg.callers[target] == nil {
			cg.callers[target] = map[string]bool{}
		}
		cg.callers[target][caller] = cg.callers[target][caller] || sure
	}
	for _, d := range decls {
		f := d.f
		for _, m := range d.methods {
			caller := methodKey(d.module, d.name, f.text(field(m, "name")))
			vars := declaredVariables(f, m)
			for _, call := range findAll(m, "method_invocation") {
				name := f.text(field(call, "name"))
				object := field(call, "object")
				class, known := "", false
				switch {
				case object == nil || object.Type(lang) == "this" || object.Type(lang) == "super":
					class, known = d.name, true
				case object.Type(lang) == "identifier":
					id := f.text(object)
					if t, ok := vars[id]; ok {
						class, known = t, t != ""
					} else if t, ok := d.fields[id]; ok {
						class, known = t, t != ""
					} else if id != "" && id[0] >= 'A' && id[0] <= 'Z' {
						class, known = id, true // a static call
					}
				case object.Type(lang) == "field_access" && f.text(field(object, "object")) == "this":
					id := f.text(field(object, "field"))
					class, known = d.fields[id], d.fields[id] != ""
				}
				if known {
					found, sure := resolve(d.module, class)
					for _, c := range found {
						link(methodKey(c.module, c.name, name), caller, sure)
					}
					continue // a known type that is not ours, such as List, is not a project call
				}
				// Unknown receiver, such as a chained call: a name only one method has is a fair guess.
				if keys := byMethodName[name]; len(keys) == 1 {
					link(keys[0], caller, false)
				}
			}
		}
	}
	cg.linkRoutes(types)
	return cg
}

var pathVariable = regexp.MustCompile(`\{[^}]*\}`)

func routeOf(e Endpoint) string { return e.Verb + " " + pathVariable.ReplaceAllString(e.Path, "{}") }

// linkRoutes records REST routes, and treats a Feign client method as a caller of the handler it reaches.
func (cg *CallGraph) linkRoutes(types []*Type) {
	handler := func(t *Type, e Endpoint) *CallMethod {
		return cg.methods[methodKey(moduleOfFile(t.File), t.Name, e.Handler)]
	}
	for _, t := range types {
		if t.Feign != "" {
			continue
		}
		for _, e := range t.Endpoints {
			if m := handler(t, e); m != nil {
				m.Routes = append(m.Routes, e.Verb+" "+e.Path)
			}
		}
	}
	for _, client := range types {
		if client.Feign == "" {
			continue
		}
		for _, server := range types {
			if server.Stereotype != "controller" || !strings.EqualFold(server.Module, client.Feign) {
				continue
			}
			for _, ce := range client.Endpoints {
				for _, se := range server.Endpoints {
					if routeOf(ce) != routeOf(se) {
						continue
					}
					from, to := handler(client, ce), handler(server, se)
					if from != nil && to != nil {
						if cg.callers[to.key] == nil {
							cg.callers[to.key] = map[string]bool{}
						}
						cg.callers[to.key][from.key] = true
					}
				}
			}
		}
	}
}

// sameMethod is a method and the same-named methods above and below its class in the hierarchy,
// which a call through an interface or superclass can reach.
func (cg *CallGraph) sameMethod(key string) []string {
	head, name, ok := strings.Cut(key, "#")
	if !ok {
		return []string{key}
	}
	out := []string{key}
	seen := map[string]bool{head: true}
	queue := []string{head}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		for _, r := range cg.related[c] {
			if !seen[r] {
				seen[r] = true
				queue = append(queue, r)
				out = append(out, r+"#"+name)
			}
		}
	}
	return out
}

// Calls is every link between two different methods, in a stable order.
func (cg *CallGraph) Calls() []MethodCall {
	best := map[[2]string]MethodCall{}
	for target, callers := range cg.callers {
		for caller, sure := range callers {
			from := cg.methods[caller]
			if from == nil {
				continue
			}
			for _, key := range cg.sameMethod(target) {
				to := cg.methods[key]
				if to == nil || key == caller {
					continue
				}
				id := [2]string{caller, key}
				if old, ok := best[id]; !ok || (sure && !old.Confirmed) {
					best[id] = MethodCall{From: from, To: to, Confirmed: sure}
				}
			}
		}
	}
	out := make([]MethodCall, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.From.key != b.From.key {
			return a.From.key < b.From.key
		}
		return a.To.key < b.To.key
	})
	return out
}
