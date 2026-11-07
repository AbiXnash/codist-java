// Package java reads Java source into types, methods, calls and Spring facts, using a pure-Go
// tree-sitter parser so Codist needs no C compiler.
package java

import (
	"regexp"
	"strconv"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

var lang = grammars.JavaLanguage()

// Type is one class, interface, enum or record.
type Type struct {
	ID          string     `json:"id"` // package.Name, or package.Outer.Inner
	Name        string     `json:"name"`
	Package     string     `json:"package"`
	File        string     `json:"file"`
	Module      string     `json:"module"`
	Kind        string     `json:"kind"`
	Stereotype  string     `json:"stereotype"` // controller, service, repository, client, entity, config, component, test, other
	Annotations []string   `json:"annotations"`
	Extends     []string   `json:"extends"`    // base types of the superclass and of interfaces an interface extends
	Implements  []string   `json:"implements"` // base types of interfaces a class implements
	Uses        []string   `json:"-"`          // every type named in the declaration, and upper-case call receivers
	Injected    []string   `json:"-"`          // types Spring injects: constructor parameters and injected fields
	Abstract    bool       `json:"-"`
	Fields      []Field    `json:"fields"`
	Methods     []Method   `json:"methods"`
	Endpoints   []Endpoint `json:"endpoints"`
	Feign       string     `json:"feign,omitempty"` // service a Feign client calls
	Imports     []string   `json:"-"`
	Line, End   int
}

// Field is a field with its declared type.
type Field struct {
	Name, Type string
	Line       int
	End        int
	Value      string // literal value of a static final constant, if any
	Signature  string // the declaration as written, without its value: "private static final Logger log"
	Names      string // all names declared together: "a, b"
	Component  bool   // a record component, which the graph does not list as a member
}

// Method is a method or constructor.
type Method struct {
	Name        string  `json:"name"`
	Line        int     `json:"line"`
	End         int     `json:"end"`
	Params      []Field `json:"params"`
	Returns     string  `json:"returns"`
	Annotations []string
	Signature   string // "public Order get(Long id)": modifiers, type and parameters, no annotations
	Calls       []Call `json:"calls"`
	Cost        Cost   `json:"cost"`
}

// Call is a method call written in a method: receiver.name(...).
type Call struct {
	Receiver string // "" for an unqualified call, "this", a field, a variable or a type
	Name     string
	Line     int
}

// Endpoint is an HTTP route a controller handles or a Feign client calls.
type Endpoint struct {
	Verb, Path, Handler string
	Line                int
}

// File is one parsed source file.
type File struct {
	Path  string
	Src   []byte
	Lines []string
	Root  *ts.Node
	Types []Type
}

// Parse reads one Java file. It never fails: a file that does not parse yields what could be read.
func Parse(path string, src []byte) *File {
	p := ts.NewParser(lang)
	tree, err := p.Parse(src)
	f := &File{Path: path, Src: src, Lines: strings.Split(string(src), "\n")}
	if err != nil || tree == nil {
		return f
	}
	f.Root = tree.RootNode()
	pkg := ""
	var imports []string
	for i := 0; i < f.Root.NamedChildCount(); i++ {
		n := f.Root.NamedChild(i)
		switch n.Type(lang) {
		case "package_declaration":
			pkg = strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(f.text(n), "package")), ";")
		case "import_declaration":
			imports = append(imports, strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(f.text(n), "import"), " static")), ";"))
		}
	}
	for i := 0; i < f.Root.NamedChildCount(); i++ {
		f.readType(f.Root.NamedChild(i), pkg, "", imports)
	}
	return f
}

func (f *File) text(n *ts.Node) string {
	if n == nil {
		return ""
	}
	return n.Text(f.Src)
}

func line(n *ts.Node) int { return int(n.StartPoint().Row) + 1 }
func endLine(n *ts.Node) int {
	return int(n.EndPoint().Row) + 1
}

func field(n *ts.Node, name string) *ts.Node { return n.ChildByFieldName(name, lang) }

func named(n *ts.Node) []*ts.Node {
	out := make([]*ts.Node, 0, n.NamedChildCount())
	for i := 0; i < n.NamedChildCount(); i++ {
		out = append(out, n.NamedChild(i))
	}
	return out
}

var typeKinds = map[string]string{"class_declaration": "class", "interface_declaration": "interface", "enum_declaration": "enum", "record_declaration": "record", "annotation_type_declaration": "annotation"}

func (f *File) readType(n *ts.Node, pkg, outer string, imports []string) {
	kind, ok := typeKinds[n.Type(lang)]
	if !ok {
		return
	}
	name := f.text(field(n, "name"))
	id := name
	if outer != "" {
		id = outer + "." + name
	}
	t := Type{ID: strings.TrimPrefix(pkg+"."+id, "."), Name: name, Package: pkg, File: f.Path, Kind: kind, Imports: imports, Line: line(n), End: endLine(n)}
	mappingBase := ""
	for _, c := range named(n) {
		switch c.Type(lang) {
		case "modifiers":
			for _, a := range named(c) {
				if strings.HasSuffix(a.Type(lang), "annotation") {
					t.Annotations = append(t.Annotations, f.text(a))
					if p := mappingPath(a, f); p != "" && annotationName(f.text(a)) == "RequestMapping" {
						mappingBase = p
					}
					if annotationName(f.text(a)) == "FeignClient" {
						t.Feign = feignTarget(f.text(a))
						if m := feignPath.FindStringSubmatch(f.text(a)); m != nil {
							mappingBase = m[1]
						}
					}
				}
			}
		case "superclass", "extends_interfaces":
			for _, x := range typeList(c) {
				if b := f.baseType(x); b != "" {
					t.Extends = append(t.Extends, b)
				}
			}
		case "super_interfaces":
			for _, x := range typeList(c) {
				if b := f.baseType(x); b != "" {
					t.Implements = append(t.Implements, b)
				}
			}
		}
	}
	t.Abstract = kind == "class" && hasModifier(n, "abstract")
	t.Uses = f.uses(n)
	t.Injected = f.injected(n, t.Annotations)
	t.Stereotype = f.stereotype(n, t, name)
	body := field(n, "body")
	if body == nil {
		f.Types = append(f.Types, t)
		return
	}
	for _, m := range members(body) {
		switch m.Type(lang) {
		case "field_declaration", "constant_declaration":
			typ := f.text(field(m, "type"))
			var decls []*ts.Node
			var names []string
			for _, d := range named(m) {
				if d.Type(lang) == "variable_declarator" {
					decls = append(decls, d)
					names = append(names, f.text(field(d, "name")))
				}
			}
			sig := f.text(m)
			if i := strings.Index(sig, "="); i > 0 {
				sig = sig[:i]
			}
			sig = strings.TrimSuffix(collapse(sig), ";")
			constant := m.Type(lang) == "constant_declaration" || hasModifier(m, "static") && hasModifier(m, "final")
			for _, d := range decls {
				fl := Field{Name: f.text(field(d, "name")), Type: typ, Line: line(m), End: endLine(m), Signature: sig, Names: strings.Join(names, ", ")}
				if v := field(d, "value"); v != nil && isLiteral(v.Type(lang)) && constant && len(decls) == 1 {
					fl.Value = f.text(v)
				}
				t.Fields = append(t.Fields, fl)
			}
		case "method_declaration", "constructor_declaration":
			t.Methods = append(t.Methods, f.readMethod(m, &t, mappingBase))
		case "record_declaration_body":
		default:
			if _, nested := typeKinds[m.Type(lang)]; nested {
				f.readType(m, pkg, id, imports)
			}
		}
	}
	if kind == "record" {
		if ps := field(n, "parameters"); ps != nil {
			for _, p := range named(ps) {
				t.Fields = append(t.Fields, Field{Name: f.text(field(p, "name")), Type: f.text(field(p, "type")), Line: line(p), Component: true})
			}
		}
	}
	f.Types = append(f.Types, t)
}

// members lists a class body's declarations. An enum keeps its fields and methods in a block after
// its constants.
func members(body *ts.Node) []*ts.Node {
	var out []*ts.Node
	for _, m := range named(body) {
		if m.Type(lang) == "enum_body_declarations" {
			out = append(out, named(m)...)
			continue
		}
		out = append(out, m)
	}
	return out
}

func isLiteral(kind string) bool {
	return strings.HasSuffix(kind, "_literal") || kind == "true" || kind == "false"
}

func (f *File) readMethod(n *ts.Node, t *Type, base string) Method {
	m := Method{Name: f.text(field(n, "name")), Line: line(n), End: endLine(n), Returns: f.text(field(n, "type")), Signature: f.signature(n)}
	if ps := field(n, "parameters"); ps != nil {
		for _, p := range named(ps) {
			if p.Type(lang) == "formal_parameter" || p.Type(lang) == "spread_parameter" {
				m.Params = append(m.Params, Field{Name: f.text(field(p, "name")), Type: f.text(field(p, "type")), Line: line(p)})
			}
		}
	}
	for _, c := range named(n) {
		if c.Type(lang) != "modifiers" {
			continue
		}
		for _, a := range named(c) {
			if !strings.HasSuffix(a.Type(lang), "annotation") {
				continue
			}
			text := f.text(a)
			m.Annotations = append(m.Annotations, text)
			if verb := verbOf(annotationName(text)); verb != "" {
				verb = requestVerb(text, verb)
				t.Endpoints = append(t.Endpoints, Endpoint{Verb: verb, Path: joinPath(base, mappingPath(a, f)), Handler: m.Name, Line: m.Line})
			}
		}
	}
	if body := field(n, "body"); body != nil {
		f.calls(body, &m)
		types := map[string]string{}
		for _, fl := range t.Fields {
			types[fl.Name] = fl.Type
		}
		for _, p := range m.Params {
			types[p.Name] = p.Type
		}
		m.Cost = measure(f, body, types)
	}
	if field(n, "body") == nil { // abstract or an interface method: nothing runs here
		m.Cost = Cost{Complexity: 1, Estimate: "abstract"}
	}
	m.Cost.Lines = m.End - m.Line + 1
	m.Cost.Params = len(m.Params)
	return m
}

func (f *File) calls(n *ts.Node, m *Method) {
	if n.Type(lang) == "method_invocation" {
		recv := f.text(field(n, "object"))
		if strings.ContainsAny(recv, "().") { // chained: keep the last name only when it is a plain field access
			if i := strings.LastIndex(recv, "."); i >= 0 && !strings.ContainsAny(recv[i+1:], "()") {
				recv = recv[i+1:]
			} else {
				recv = "?"
			}
		}
		m.Calls = append(m.Calls, Call{Receiver: recv, Name: f.text(field(n, "name")), Line: line(n)})
	}
	for _, c := range named(n) {
		f.calls(c, m)
	}
}

// AnnotationName is an annotation's name without @, package or arguments: @a.b.Service("x") is Service.
func AnnotationName(text string) string { return annotationName(text) }

func annotationName(text string) string {
	s := strings.TrimPrefix(text, "@")
	if i := strings.IndexAny(s, "( "); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func verbOf(annotation string) string {
	switch annotation {
	case "GetMapping":
		return "GET"
	case "PostMapping":
		return "POST"
	case "PutMapping":
		return "PUT"
	case "DeleteMapping":
		return "DELETE"
	case "PatchMapping":
		return "PATCH"
	case "RequestMapping":
		return "ANY"
	}
	return ""
}

var methodAttr = regexp.MustCompile(`RequestMethod\.([A-Z]+)`)

func requestVerb(text, verb string) string {
	if m := methodAttr.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return verb
}

var quoted = regexp.MustCompile(`"((?:\\.|[^"\\])*)"`)

// mappingPath reads the first path from a mapping annotation: @GetMapping("/x"), value=, path=.
func mappingPath(n *ts.Node, f *File) string {
	text := f.text(n)
	if !strings.Contains(text, "(") {
		return ""
	}
	args := text[strings.Index(text, "(")+1:]
	for _, key := range []string{"value", "path"} {
		if i := regexp.MustCompile(`\b` + key + `\s*=`).FindStringIndex(args); i != nil {
			if m := quoted.FindStringSubmatch(args[i[1]:]); m != nil {
				return m[1]
			}
		}
	}
	if strings.Contains(args, "=") && !strings.HasPrefix(strings.TrimSpace(args), `"`) && !strings.HasPrefix(strings.TrimSpace(args), "{") {
		return ""
	}
	if m := quoted.FindStringSubmatch(args); m != nil {
		return m[1]
	}
	return ""
}

func joinPath(base, p string) string {
	if base == "" && p == "" {
		return "/"
	}
	out := "/" + strings.Trim(base, "/")
	if p = strings.Trim(p, "/"); p != "" {
		out = strings.TrimSuffix(out, "/") + "/" + p
	}
	return out
}

var feignPath = regexp.MustCompile(`\bpath\s*=\s*"([^"]*)"`)

var feignName = regexp.MustCompile(`(?:name|value)\s*=\s*"([^"]+)"|^@FeignClient\s*\(\s*"([^"]+)"`)

func feignTarget(text string) string {
	if m := feignName.FindStringSubmatch(text); m != nil {
		if m[1] != "" {
			return m[1]
		}
		return m[2]
	}
	return ""
}

var repoBase = regexp.MustCompile(`^((Jpa|Crud|PagingAndSorting|ListCrud|ListPagingAndSorting|Mongo|Reactive\w*|R2dbc)Repository|Repository)$`)

// stereotype is a class's role: its Spring annotation first, then a repository interface it
// extends, then its name, then whether it is a test. The order matches the first Codist.
func (f *File) stereotype(n *ts.Node, t Type, name string) string {
	for _, a := range t.Annotations {
		switch annotationName(a) {
		case "RestController", "Controller":
			return "controller"
		case "RestControllerAdvice", "ControllerAdvice":
			return "advice"
		case "FeignClient":
			return "client"
		case "Service":
			return "service"
		case "Repository":
			return "repository"
		case "Entity", "Embeddable", "MappedSuperclass", "Document", "Table":
			return "entity"
		case "Configuration", "SpringBootApplication", "EnableAutoConfiguration", "ConfigurationProperties":
			return "config"
		case "Component", "Mapper":
			return "component"
		}
	}
	if t.Kind == "interface" {
		for _, b := range t.Extends {
			if repoBase.MatchString(b) {
				return "repository"
			}
		}
	}
	switch {
	case strings.HasSuffix(name, "Dto"), strings.HasSuffix(name, "DTO"), strings.HasSuffix(name, "Request"), strings.HasSuffix(name, "Response"):
		return "dto"
	case strings.HasSuffix(name, "Exception"):
		return "exception"
	case strings.Contains(f.Path, "src/test/"):
		return "test"
	}
	if body := field(n, "body"); body != nil {
		for _, m := range members(body) {
			if m.Type(lang) == "method_declaration" && hasAnnotationNode(f, m, "Test") {
				return "test"
			}
		}
	}
	return "other"
}

func hasAnnotationNode(f *File, n *ts.Node, name string) bool {
	for _, c := range named(n) {
		if c.Type(lang) != "modifiers" {
			continue
		}
		for _, a := range named(c) {
			if strings.HasSuffix(a.Type(lang), "annotation") && annotationName(f.text(a)) == name {
				return true
			}
		}
	}
	return false
}

func hasModifier(n *ts.Node, word string) bool {
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.Type(lang) != "modifiers" {
			continue
		}
		for j := 0; j < c.ChildCount(); j++ {
			if c.Child(j).Type(lang) == word {
				return true
			}
		}
	}
	return false
}

// typeList is the types in a superclass, implements or extends clause.
func typeList(n *ts.Node) []*ts.Node {
	for _, c := range named(n) {
		if c.Type(lang) == "type_list" {
			return named(c)
		}
	}
	return named(n)
}

// baseType is the simple name of a type without its arguments: List<Order> is List, a.b.C is C.
func (f *File) baseType(t *ts.Node) string {
	if t == nil {
		return ""
	}
	switch t.Type(lang) {
	case "type_identifier":
		return f.text(t)
	case "generic_type":
		for _, c := range named(t) {
			if c.Type(lang) == "type_identifier" || c.Type(lang) == "scoped_type_identifier" {
				return f.baseType(c)
			}
		}
	case "scoped_type_identifier":
		last := ""
		for _, c := range named(t) {
			if c.Type(lang) == "type_identifier" {
				last = f.text(c)
			}
		}
		return last
	case "array_type":
		return f.baseType(field(t, "element"))
	}
	return ""
}

// uses lists every type named anywhere in a declaration, and the upper-case receivers of calls and
// field reads, which are static calls and constants.
func (f *File) uses(n *ts.Node) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var walk func(x *ts.Node)
	walk = func(x *ts.Node) {
		switch x.Type(lang) {
		case "type_identifier":
			add(f.text(x))
		case "method_invocation", "field_access":
			if obj := field(x, "object"); obj != nil && obj.Type(lang) == "identifier" {
				if s := f.text(obj); s != "" && s[0] >= 'A' && s[0] <= 'Z' {
					add(s)
				}
			}
		}
		for i := 0; i < x.NamedChildCount(); i++ {
			walk(x.NamedChild(i))
		}
	}
	walk(n)
	return out
}

// injected lists the types Spring injects: every constructor parameter, and fields marked
// @Autowired, @Inject or @Resource, or final fields when Lombok writes the constructor.
func (f *File) injected(n *ts.Node, annotations []string) []string {
	body := field(n, "body")
	if body == nil {
		return nil
	}
	lombok := false
	for _, a := range annotations {
		if x := annotationName(a); x == "RequiredArgsConstructor" || x == "AllArgsConstructor" {
			lombok = true
		}
	}
	var out []string
	for _, m := range members(body) {
		switch m.Type(lang) {
		case "constructor_declaration":
			if ps := field(m, "parameters"); ps != nil {
				for _, p := range named(ps) {
					if p.Type(lang) == "formal_parameter" {
						out = append(out, f.baseType(field(p, "type")))
					}
				}
			}
		case "field_declaration":
			if hasModifier(m, "static") {
				continue
			}
			if hasAnnotationNode(f, m, "Autowired") || hasAnnotationNode(f, m, "Inject") || hasAnnotationNode(f, m, "Resource") || lombok && hasModifier(m, "final") {
				out = append(out, f.baseType(field(m, "type")))
			}
		}
	}
	return out
}

// signature is a member's declaration without annotations and body: "public Order get(Long id)".
func (f *File) signature(m *ts.Node) string {
	var parts []string
	for _, c := range named(m) {
		if c.Type(lang) != "modifiers" {
			continue
		}
		for i := 0; i < c.ChildCount(); i++ {
			k := c.Child(i)
			if k.Type(lang) != "annotation" && k.Type(lang) != "marker_annotation" {
				parts = append(parts, f.text(k))
			}
		}
	}
	if tp := field(m, "type_parameters"); tp != nil {
		parts = append(parts, f.text(tp))
	}
	if t := field(m, "type"); t != nil {
		parts = append(parts, f.text(t))
	}
	parts = append(parts, f.text(field(m, "name"))+f.text(field(m, "parameters")))
	return collapse(strings.Join(parts, " "))
}

var spaces = regexp.MustCompile(`\s+`)

func collapse(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// Lines counts lines of a type for size reports.
func (t Type) Lines() int { return t.End - t.Line + 1 }

func itoa(n int) string { return strconv.Itoa(n) }
