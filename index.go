package java

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Module is a build module: a directory with a pom.xml or build.gradle. In a microservice repo
// each Spring Boot application is one module.
type Module struct {
	Name      string   `json:"name"`
	Dir       string   `json:"dir"`
	App       string   `json:"app"`  // spring.application.name, how Feign clients name it
	Port      string   `json:"port"` // server.port
	Kind      string   `json:"kind"` // service or library
	Types     int      `json:"types"`
	Lines     int      `json:"lines"`
	Tests     int      `json:"tests"`
	Endpoints int      `json:"endpoints"`
	Calls     []string `json:"calls"`    // modules it calls through Feign
	CalledBy  []string `json:"calledBy"` // modules that call it
	Uses      []string `json:"uses"`     // library modules whose classes it uses
}

// Link is a resolved call from one method to another, or to a type when the method is not
// declared there (a Spring Data repository method, for one).
type Link struct {
	From, To string // Type.ID#method, or Type.ID
	Line     int
}

// Index is the whole repository at one commit.
type Index struct {
	Types   []*Type
	ByID    map[string]*Type
	byName  map[string][]*Type
	Modules []*Module
	Links   []Link
	Lines   int
	Files   int
	Sources map[string][]byte // the Java files, kept for the call graph
}

var buildFiles = []string{"pom.xml", "build.gradle", "build.gradle.kts"}

// Build parses every Java file in sources (path to content) and resolves calls between them.
// other holds the non-Java files that matter: build files and Spring configuration.
func Build(sources map[string][]byte, other map[string][]byte) *Index {
	ix := &Index{ByID: map[string]*Type{}, byName: map[string][]*Type{}, Sources: sources}
	dirs := map[string]*Module{}
	for p := range other {
		for _, b := range buildFiles {
			if path.Base(p) == b {
				d := path.Dir(p)
				if d == "." {
					d = ""
				}
				dirs[d] = &Module{Name: moduleName(d), Dir: d, Kind: "library"}
			}
		}
	}
	for p, src := range other {
		if m := moduleOf(p, dirs); m != nil && isAppConfig(p) {
			readAppConfig(string(src), m)
		}
	}
	// Parse files in parallel; the parser is the slow part.
	paths := make([]string, 0, len(sources))
	for p := range sources {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	files := make([]*File, len(paths))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				files[i] = Parse(paths[i], sources[paths[i]])
			}
		}()
	}
	for i := range paths {
		next <- i
	}
	close(next)
	wg.Wait()
	for _, f := range files {
		ix.Files++
		ix.Lines += len(f.Lines)
		m := moduleOf(f.Path, dirs)
		for i := range f.Types {
			t := &f.Types[i]
			if m != nil {
				t.Module = m.Name
				m.Types++
				m.Lines += t.Lines()
				if t.Stereotype != "client" { // a Feign client's routes are calls out, not routes served
					m.Endpoints += len(t.Endpoints)
				}
				if t.Stereotype == "test" {
					m.Tests++
				}
				if t.Stereotype == "config" && hasAnnotation(t.Annotations, "SpringBootApplication") {
					m.Kind = "service"
				}
			}
			ix.Types = append(ix.Types, t)
			ix.ByID[t.ID] = t
			ix.byName[t.Name] = append(ix.byName[t.Name], t)
		}
	}
	for _, m := range dirs {
		if m.Types > 0 {
			ix.Modules = append(ix.Modules, m)
		}
	}
	sort.Slice(ix.Modules, func(i, j int) bool { return ix.Modules[i].Name < ix.Modules[j].Name })
	ix.resolve()
	ix.connectModules()
	return ix
}

func moduleName(dir string) string {
	if dir == "" {
		return "root"
	}
	return path.Base(dir)
}

func moduleOf(p string, dirs map[string]*Module) *Module {
	for d := path.Dir(p); ; d = path.Dir(d) {
		key := d
		if key == "." || key == "/" {
			key = ""
		}
		if m, ok := dirs[key]; ok {
			return m
		}
		if key == "" {
			return nil
		}
	}
}

func isAppConfig(p string) bool {
	b := path.Base(p)
	return strings.Contains(p, "src/main/resources/") && (b == "application.yml" || b == "application.yaml" || b == "application.properties")
}

var (
	propPort = regexp.MustCompile(`(?m)^server\.port\s*[=:]\s*(\S+)`)
	propName = regexp.MustCompile(`(?m)^spring\.application\.name\s*[=:]\s*(\S+)`)
)

// readAppConfig reads the port and application name from application.yml or .properties.
// YAML is read line by line by indentation, which is enough for these two keys.
func readAppConfig(src string, m *Module) {
	if v := propPort.FindStringSubmatch(src); v != nil {
		m.Port = v[1]
	}
	if v := propName.FindStringSubmatch(src); v != nil {
		m.App = v[1]
	}
	var stack []string
	var indents []int
	for _, raw := range strings.Split(src, "\n") {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		for len(indents) > 0 && indents[len(indents)-1] >= indent {
			stack, indents = stack[:len(stack)-1], indents[:len(indents)-1]
		}
		key, value, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		full := strings.Join(append(append([]string{}, stack...), key), ".")
		switch {
		case value == "":
			stack, indents = append(stack, key), append(indents, indent)
		case full == "server.port":
			m.Port = value
		case full == "spring.application.name":
			m.App = value
		}
	}
}

func hasAnnotation(as []string, name string) bool {
	for _, a := range as {
		if annotationName(a) == name {
			return true
		}
	}
	return false
}

var generic = regexp.MustCompile(`<.*>|\[\]|\.\.\.`)

// simpleType turns "List<Order>" into "List" and "com.x.Order" into "Order".
func simpleType(t string) string {
	t = generic.ReplaceAllString(strings.TrimSpace(t), "")
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// Lookup finds the type a simple name means from inside from: same package first, then imports,
// then a unique match anywhere.
func (ix *Index) Lookup(name string, from *Type) *Type {
	cands := ix.byName[simpleType(name)]
	if len(cands) == 0 {
		return nil
	}
	if len(cands) == 1 {
		return cands[0]
	}
	if from != nil {
		for _, c := range cands {
			if c.Package == from.Package && c.Module == from.Module {
				return c
			}
		}
		for _, c := range cands {
			for _, imp := range from.Imports {
				if imp == c.ID || strings.TrimSuffix(imp, ".*") == c.Package {
					return c
				}
			}
		}
		for _, c := range cands {
			if c.Module == from.Module {
				return c
			}
		}
	}
	return nil
}

// resolve turns each written call into a link where the receiver's type can be told.
func (ix *Index) resolve() {
	for _, t := range ix.Types {
		fields := map[string]string{}
		for _, f := range t.Fields {
			fields[f.Name] = f.Type
		}
		for _, m := range t.Methods {
			params := map[string]string{}
			for _, p := range m.Params {
				params[p.Name] = p.Type
			}
			for _, c := range m.Calls {
				var target *Type
				switch {
				case c.Receiver == "" || c.Receiver == "this":
					target = t
				case params[c.Receiver] != "":
					target = ix.Lookup(params[c.Receiver], t)
				case fields[c.Receiver] != "":
					target = ix.Lookup(fields[c.Receiver], t)
				case c.Receiver != "?" && c.Receiver[0] >= 'A' && c.Receiver[0] <= 'Z':
					target = ix.Lookup(c.Receiver, t)
				}
				if target == nil {
					continue
				}
				to := target.ID
				if target.Method(c.Name) != nil {
					to += "#" + c.Name
				}
				ix.Links = append(ix.Links, Link{From: t.ID + "#" + m.Name, To: to, Line: c.Line})
			}
		}
	}
}

// Method finds a method by name.
func (t *Type) Method(name string) *Method {
	for i := range t.Methods {
		if t.Methods[i].Name == name {
			return &t.Methods[i]
		}
	}
	return nil
}

// connectModules records which services call which through Feign, and which libraries each uses.
func (ix *Index) connectModules() {
	byApp := map[string]*Module{}
	for _, m := range ix.Modules {
		byApp[m.Name] = m
		if m.App != "" {
			byApp[m.App] = m
		}
	}
	add := func(list []string, v string) []string {
		for _, x := range list {
			if x == v {
				return list
			}
		}
		return append(list, v)
	}
	mod := func(name string) *Module {
		for _, m := range ix.Modules {
			if m.Name == name {
				return m
			}
		}
		return nil
	}
	for _, t := range ix.Types {
		from := mod(t.Module)
		if from == nil {
			continue
		}
		if t.Feign != "" {
			if to := byApp[t.Feign]; to != nil && to != from {
				from.Calls = add(from.Calls, to.Name)
				to.CalledBy = add(to.CalledBy, from.Name)
			}
		}
	}
	for _, l := range ix.Links {
		a, b := ix.ByID[strings.Split(l.From, "#")[0]], ix.ByID[strings.Split(l.To, "#")[0]]
		if a == nil || b == nil || a.Module == b.Module {
			continue
		}
		if m, lib := mod(a.Module), mod(b.Module); m != nil && lib != nil && lib.Kind == "library" {
			m.Uses = add(m.Uses, lib.Name)
		}
	}
	for _, m := range ix.Modules {
		sort.Strings(m.Calls)
		sort.Strings(m.CalledBy)
		sort.Strings(m.Uses)
	}
}

// Module finds a module by name.
func (ix *Index) Module(name string) *Module {
	for _, m := range ix.Modules {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// TypeAt finds the innermost type and method covering a line of a file.
func (ix *Index) TypeAt(file string, line int) (*Type, *Method) {
	var best *Type
	for _, t := range ix.Types {
		if t.File == file && t.Line <= line && line <= t.End && (best == nil || t.Line >= best.Line) {
			best = t
		}
	}
	if best == nil {
		return nil, nil
	}
	for i := range best.Methods {
		if m := &best.Methods[i]; m.Line <= line && line <= m.End {
			return best, m
		}
	}
	return best, nil
}

// Callers lists the methods that call typeID#method.
func (ix *Index) Callers(target string) []Link {
	var out []Link
	for _, l := range ix.Links {
		if l.To == target && !strings.HasPrefix(l.From, strings.Split(target, "#")[0]+"#") {
			out = append(out, l)
		}
	}
	return out
}

// Resolve finds the class a simple name means inside from, the way the first Codist did: an
// import names it exactly; otherwise the same package, then wildcard imports, then a class nested
// in this package, then the only class of that name. Classes outside the project resolve to "".
func (ix *Index) Resolve(simple string, from *Type) string {
	if simple == "" {
		return ""
	}
	for _, imp := range from.Imports {
		if strings.HasSuffix(imp, "."+simple) {
			if ix.ByID[imp] != nil {
				return imp
			}
			return ""
		}
	}
	cands := ix.byName[simple]
	if len(cands) == 0 {
		return ""
	}
	if t := ix.ByID[from.Package+"."+simple]; t != nil {
		return t.ID
	}
	for _, imp := range from.Imports {
		if star, ok := strings.CutSuffix(imp, ".*"); ok {
			if t := ix.ByID[star+"."+simple]; t != nil {
				return t.ID
			}
		}
	}
	for _, c := range cands { // a nested class in this package
		if rest, ok := strings.CutPrefix(c.ID, from.Package+"."); ok && strings.Contains(rest, ".") {
			return c.ID
		}
	}
	if len(cands) == 1 {
		return cands[0].ID
	}
	var same []*Type
	for _, c := range cands {
		if c.Module == from.Module {
			same = append(same, c)
		}
	}
	if len(same) == 1 {
		return same[0].ID
	}
	return ""
}
