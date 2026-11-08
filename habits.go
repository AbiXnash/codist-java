package java

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// Habits are the project's own ways of doing things, learned from its code with no model and no labels: a value that
// nearly always goes through the same helper, a type nearly always made through its factory, a filter nearly every
// query on a table has. Code that breaks a strong habit is often a mistake the project's rules would catch, such as
// an email not lower-cased in one export or deleted rows read by one query.
type Habits struct {
	Wraps     map[string]*Habit // type and accessor, such as "Employee.email" -> its usual helper, such as "Emails.normal"
	Factories map[string]*Habit // type name, such as "Paging" -> its usual factory, such as "Paging.of"
	Filters   map[string]*Habit // table, such as "employee" -> a filter nearly every query on it has
}

// Habit is one learned habit: what is usually done, and how often it was seen.
type Habit struct {
	Usual     string // the helper, factory or filter
	Kept, All int    // how many uses follow it, out of all uses
}

// A habit counts only when it is seen often enough and broken rarely, so one-off styles are not taken for rules. A
// query filter may be broken more often before it stops counting: leaving out a filter such as deleted_at IS NULL is
// costly, and the queries that need every row are few. Measured on jsoup, gson, commons-lang, jackson-databind and a
// 791-file Spring project: at 0.75 for code habits only one weak habit was left (jsoup's Document.createShell, 5/8).
const (
	habitMinKept        = 3
	habitMinShare       = 0.75
	habitMinFilterShare = 0.6
)

func (h *Habit) strong() bool { return h.strongAt(habitMinShare) }

func (h *Habit) strongAt(share float64) bool {
	return h.Kept >= habitMinKept && h.Kept < h.All && float64(h.Kept)/float64(h.All) >= share
}

var (
	fromTable   = regexp.MustCompile(`(?i)\bFROM\s+([A-Za-z_][A-Za-z0-9_.]*)`)
	whereClause = regexp.MustCompile(`(?is)\bWHERE\b(.*?)(\bGROUP\s+BY\b|\bORDER\s+BY\b|\bLIMIT\b|\bHAVING\b|$)`)
	andSplit    = regexp.MustCompile(`(?i)\s+AND\s+`)
	sqlSpaces   = regexp.MustCompile(`\s+`)
)

// MineHabits learns the habits of a project from its main Java files.
func MineHabits(files []*File) *Habits {
	wraps := map[string]map[string]int{}   // accessor -> helper -> uses through it ("" = used raw)
	builds := map[string]map[string]int{}  // type -> factory -> uses ("new" = constructor)
	filters := map[string]map[string]int{} // table -> filter -> queries with it ("" = every query)
	factories := factoryMethods(files)
	project := map[string]bool{} // the project's own types: only their helpers make a habit, not Long.parseLong
	for _, f := range files {
		for _, t := range f.Types {
			project[t.Name] = true
		}
	}
	for _, f := range files {
		if f.Root == nil {
			continue
		}
		consts := constants(f)
		own := map[string]bool{}
		for _, t := range f.Types {
			own[t.Name] = true
		}
		walkAll(f.Root, func(n *ts.Node) {
			switch n.Type(lang) {
			case "method_invocation":
				if key, helper, ok := accessorUse(f, n); ok && (helper == "" || project[helper[:strings.Index(helper, ".")]]) {
					count(wraps, key, helper)
				}
				if t, m := f.text(field(n, "object")), f.text(field(n, "name")); factories[t+"."+m] && !own[t] {
					count(builds, t, t+"."+m)
				}
				if table, conds, ok := query(f, n, consts); ok {
					count(filters, table, "")
					for c := range conds {
						count(filters, table, c)
					}
				}
			case "object_creation_expression":
				if anonymous(n) {
					return // new TypeToken<List<X>>() {} makes a subclass: the idiom, not a way around a factory
				}
				if t := simpleType(f.text(field(n, "type"))); hasFactory(factories, t) && !own[t] {
					count(builds, t, "new")
				}
			}
		})
	}
	h := &Habits{Wraps: map[string]*Habit{}, Factories: map[string]*Habit{}, Filters: map[string]*Habit{}}
	for name, by := range wraps {
		if best := usual(by, ""); best != nil {
			h.Wraps[name] = best
		}
	}
	for t, by := range builds {
		if best := usual(by, "new"); best != nil {
			h.Factories[t] = best
		}
	}
	for table, by := range filters {
		all := by[""]
		var names []string
		for c := range by {
			if c != "" {
				names = append(names, c)
			}
		}
		sort.Strings(names)
		for _, c := range names { // the most common filter, ties by name so the result does not change between runs
			if x := (&Habit{Usual: c, Kept: by[c], All: all}); x.strongAt(habitMinFilterShare) && (h.Filters[table] == nil || x.Kept > h.Filters[table].Kept) {
				h.Filters[table] = x
			}
		}
	}
	return h
}

// habitIssues reports, on the given lines, code that breaks one of the project's strong habits.
func (c *checker) habitIssues(h *Habits) {
	if h == nil || c.test || c.f.Root == nil {
		return
	}
	consts := constants(c.f)
	walkAll(c.f.Root, func(n *ts.Node) {
		switch n.Type(lang) {
		case "method_invocation":
			if key, helper, ok := accessorUse(c.f, n); ok && helper == "" {
				if x := h.Wraps[key]; x != nil {
					name := key[strings.LastIndex(key, ".")+1:]
					c.addAt(line(n), line(n), "habit-wrap", "medium", fmt.Sprintf("%s() used without %s", name, x.Usual),
						fmt.Sprintf("Elsewhere in this project %s() goes through %s (%d of %d uses). Used raw here, this value may not match the others.", key, x.Usual, x.Kept, x.All),
						fmt.Sprintf("%s(%s())", x.Usual, c.f.text(field(n, "object"))+"."+name))
				}
			}
			if table, conds, ok := query(c.f, n, consts); ok {
				if x := h.Filters[table]; x != nil && !conds[x.Usual] {
					c.addAt(line(n), endLine(n), "habit-filter", "high", "Query on "+table+" without "+x.Usual,
						fmt.Sprintf("%d of %d queries on %s in this project filter %s. This one does not, so it may read rows the others leave out.", x.Kept, x.All, table, x.Usual),
						"Add "+x.Usual+" to the WHERE clause, or say in a comment why this query needs every row.")
				}
			}
		case "object_creation_expression":
			if anonymous(n) {
				return
			}
			t := simpleType(c.f.text(field(n, "type")))
			for _, typ := range c.f.Types {
				if typ.Name == t {
					return // a type builds itself however it likes
				}
			}
			if x := h.Factories[t]; x != nil {
				c.addAt(line(n), line(n), "habit-factory", "medium", "new "+t+"(...) instead of "+x.Usual,
					fmt.Sprintf("Elsewhere in this project %s objects are made with %s (%d of %d). The factory may check or fix its inputs, which new skips.", t, x.Usual, x.Kept, x.All),
					x.Usual+"(...)")
			}
		}
	})
}

// usual is the most common way among by, other than raw, when it is a strong habit.
func usual(by map[string]int, raw string) *Habit {
	all := 0
	for _, n := range by {
		all += n
	}
	var best *Habit
	var ways []string
	for w := range by {
		ways = append(ways, w)
	}
	sort.Strings(ways)
	for _, w := range ways {
		if w != raw && (best == nil || by[w] > best.Kept) {
			best = &Habit{Usual: w, Kept: by[w], All: all}
		}
	}
	if best == nil || !best.strong() {
		return nil
	}
	return best
}

func anonymous(n *ts.Node) bool {
	for _, k := range named(n) {
		if k.Type(lang) == "class_body" {
			return true
		}
	}
	return false
}

func count(m map[string]map[string]int, k, v string) {
	if m[k] == nil {
		m[k] = map[string]int{}
	}
	m[k][v]++
}

func walkAll(n *ts.Node, visit func(*ts.Node)) {
	visit(n)
	for _, k := range named(n) {
		walkAll(k, visit)
	}
}

// accessorUse reads a call such as e.email() with no arguments on a variable of a known type: an accessor. key is
// the type and accessor, such as "Employee.email"; helper is the static method it is passed straight to, such as
// "Emails.normal" for Emails.normal(e.email()), or "" when used raw. The same name on another type is another habit:
// a view's email() may already be lower case where the stored one is not.
func accessorUse(f *File, n *ts.Node) (key, helper string, ok bool) {
	obj := field(n, "object")
	if obj == nil || obj.Type(lang) != "identifier" || constantName.MatchString(f.text(obj)) {
		return "", "", false
	}
	typ := varType(f, n, f.text(obj))
	if typ == "" {
		return "", "", false
	}
	if args := field(n, "arguments"); args == nil || args.NamedChildCount() != 0 {
		return "", "", false
	}
	name := f.text(field(n, "name"))
	key = typ + "." + name
	if objectMethod[name] || strings.HasPrefix(name, "is") || name == "size" || name == "length" || name == "isEmpty" ||
		name == "stream" || name == "iterator" || name == "get" || name == "build" {
		return "", "", false
	}
	if p := n.Parent(); p != nil && p.Type(lang) == "argument_list" && p.NamedChildCount() == 1 {
		if call := p.Parent(); call != nil && call.Type(lang) == "method_invocation" {
			if o := field(call, "object"); o != nil && o.Type(lang) == "identifier" && isTypeName(f.text(o)) {
				return key, f.text(o) + "." + f.text(field(call, "name")), true
			}
		}
	}
	return key, "", true
}

// varType is the declared type of the variable name as seen from n: a parameter, local variable or loop variable of
// the enclosing method or lambda, or a field of the class. It is "" when the type is not written, as for var or a
// lambda parameter, so a habit is never guessed from an unknown type.
func varType(f *File, n *ts.Node, name string) string {
	for p := n.Parent(); p != nil; p = p.Parent() {
		var found string
		switch p.Type(lang) {
		case "class_body":
			for _, d := range named(p) {
				if d.Type(lang) == "field_declaration" {
					for _, v := range named(d) {
						if v.Type(lang) == "variable_declarator" && f.text(field(v, "name")) == name {
							return simpleType(f.text(field(d, "type")))
						}
					}
				}
			}
		case "method_declaration", "constructor_declaration", "lambda_expression":
			walkAll(p, func(d *ts.Node) {
				if found != "" {
					return
				}
				switch d.Type(lang) {
				case "formal_parameter", "enhanced_for_statement":
					if f.text(field(d, "name")) == name {
						found = f.text(field(d, "type"))
					}
				case "local_variable_declaration":
					for _, v := range named(d) {
						if v.Type(lang) == "variable_declarator" && f.text(field(v, "name")) == name {
							found = f.text(field(d, "type"))
						}
					}
				}
			})
			if found != "" {
				if t := simpleType(found); t != "var" {
					return t
				}
				return ""
			}
			if p.Type(lang) == "lambda_expression" {
				if ps := field(p, "parameters"); ps != nil && strings.Contains(" "+f.text(ps)+" ", name) {
					return "" // an untyped lambda parameter
				}
			}
		}
	}
	return ""
}

func isTypeName(s string) bool {
	return s != "" && s[0] >= 'A' && s[0] <= 'Z' && !constantName.MatchString(s)
}

// factoryMethods names the static methods of project types that build and return their own type, such as
// "Paging.of". A static method returning its type without building one, such as a cast or a lookup, is not a factory.
func factoryMethods(files []*File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, t := range f.Types {
			for _, m := range t.Methods {
				body := ""
				if m.Line >= 1 && m.End <= len(f.Lines) && m.Line <= m.End {
					body = strings.Join(f.Lines[m.Line-1:m.End], "\n")
				}
				builds := strings.Contains(body, "new "+t.Name+"(") || strings.Contains(body, "new "+t.Name+"<")
				if simpleType(m.Returns) == t.Name && strings.Contains(" "+m.Signature+" ", " static ") && builds {
					out[t.Name+"."+m.Name] = true
				}
			}
		}
	}
	return out
}

func hasFactory(factories map[string]bool, t string) bool {
	for k := range factories {
		if strings.HasPrefix(k, t+".") {
			return true
		}
	}
	return false
}

// constants maps the file's constants to their text: static final String fields set from literals and other such
// constants, such as SELECT = "SELECT id, name " + "FROM employee".
func constants(f *File) map[string]string {
	out := map[string]string{}
	walkAll(f.Root, func(n *ts.Node) {
		if n.Type(lang) != "field_declaration" || !strings.Contains(f.text(n), "final") {
			return
		}
		for _, d := range named(n) {
			if d.Type(lang) == "variable_declarator" {
				if v := field(d, "value"); v != nil {
					if s, ok := sqlText(f, v, out); ok {
						out[f.text(field(d, "name"))] = s
					}
				}
			}
		}
	})
	return out
}

// sqlText is the text of a string expression made of literals and known constants. ok is false when any part is
// not known, such as a parameter: the query text is then not fully known.
func sqlText(f *File, n *ts.Node, consts map[string]string) (string, bool) {
	switch n.Type(lang) {
	case "string_literal":
		return strings.Trim(f.text(n), `"`), true
	case "identifier":
		s, ok := consts[f.text(n)]
		return s, ok
	case "parenthesized_expression":
		if n.NamedChildCount() == 1 {
			return sqlText(f, n.NamedChild(0), consts)
		}
	case "binary_expression":
		if f.text(field(n, "operator")) == "+" {
			l, ok1 := sqlText(f, field(n, "left"), consts)
			r, ok2 := sqlText(f, field(n, "right"), consts)
			return l + r, ok1 && ok2
		}
	}
	return "", false
}

// query reads a SQL call such as jdbc.sql("SELECT ... FROM employee WHERE a AND b"): the table and the filters, each
// with spacing and case made even. Parts of the text that are not known still count when the table is.
func query(f *File, n *ts.Node, consts map[string]string) (string, map[string]bool, bool) {
	if !sqlMethods[f.text(field(n, "name"))] {
		return "", nil, false
	}
	args := field(n, "arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return "", nil, false
	}
	var text strings.Builder
	walkAll(args.NamedChild(0), func(k *ts.Node) {
		switch k.Type(lang) {
		case "string_literal":
			text.WriteString(strings.Trim(f.text(k), `"`))
		case "identifier":
			text.WriteString(consts[f.text(k)])
		}
	})
	sql := text.String()
	m := fromTable.FindStringSubmatch(sql)
	if m == nil || !regexp.MustCompile(`(?i)^\s*SELECT\b`).MatchString(sql) {
		return "", nil, false
	}
	conds := map[string]bool{}
	if w := whereClause.FindStringSubmatch(sql); w != nil {
		for _, c := range andSplit.Split(w[1], -1) {
			c = strings.ToLower(strings.TrimSpace(sqlSpaces.ReplaceAllString(strings.Trim(strings.TrimSpace(c), "()"), " ")))
			if c != "" && !strings.ContainsAny(c, ":?") { // a filter on a parameter is a choice per query, not a habit
				conds[c] = true
			}
		}
	}
	return strings.ToLower(m[1]), conds, true
}
