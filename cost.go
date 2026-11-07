package java

import (
	"regexp"
	"strconv"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// Cost is estimated by reading a method, not by running it.
type Cost struct {
	Complexity int    `json:"complexity"`  // paths through the method: branches + 1
	LoopDepth  int    `json:"loopDepth"`   // deepest loop nesting
	DBCalls    int    `json:"dbCalls"`     // repository or JDBC calls
	Remote     int    `json:"remoteCalls"` // HTTP, Feign and messaging calls
	LoopIO     int    `json:"loopIo"`      // of those, how many repeat inside a loop
	Lines      int    `json:"lines"`
	Params     int    `json:"params"`
	Estimate   string `json:"estimate"` // O(1), O(n), O(n²), O(n) round trips
}

var (
	dbReceiver   = regexp.MustCompile(`(?i)(repository|repo|dao|jdbctemplate|entitymanager)$|^em$`)
	dbMethod     = regexp.MustCompile(`^(find|save|delete|exists|count|get(One|ReferenceById)|query|update|persist|merge|execute|createQuery|createNativeQuery|batchUpdate)`)
	remoteObject = regexp.MustCompile(`(?i)(client|resttemplate|kafkatemplate|rabbittemplate|redistemplate|mongotemplate|webclient|restclient|feign|gateway|api|http|stub|proxy)$`)
	loopTypes    = map[string]bool{"for_statement": true, "enhanced_for_statement": true, "while_statement": true, "do_statement": true}
	branchTypes  = map[string]bool{"if_statement": true, "for_statement": true, "enhanced_for_statement": true, "while_statement": true,
		"do_statement": true, "catch_clause": true, "ternary_expression": true, "switch_label": true}
	streamSteps = map[string]bool{"map": true, "filter": true, "flatMap": true, "anyMatch": true, "allMatch": true, "noneMatch": true, "forEach": true}
)

// runsOnce says whether kid is evaluated once however many times its parent repeats: what a for-each
// walks over, a for loop's setup, or what a call is made on.
func runsOnce(n, kid *ts.Node) bool {
	var once *ts.Node
	switch n.Type(lang) {
	case "enhanced_for_statement":
		once = field(n, "value")
	case "for_statement":
		once = field(n, "init")
	case "method_invocation":
		once = field(n, "object")
	}
	return once != nil && once.StartByte() == kid.StartByte() && once.EndByte() == kid.EndByte()
}

// smallFixedLoop is a counting loop with a small literal limit, such as i < 3: it repeats a set
// number of times however much data there is.
func smallFixedLoop(f *File, n *ts.Node) bool {
	if n.Type(lang) != "for_statement" {
		return false
	}
	cond := field(n, "condition")
	if cond == nil || cond.Type(lang) != "binary_expression" {
		return false
	}
	limit := field(cond, "right")
	if limit == nil || limit.Type(lang) != "decimal_integer_literal" {
		return false
	}
	v, err := strconv.Atoi(f.text(limit))
	return err == nil && v <= 16
}

// measure estimates one method. types maps fields and parameters to their declared types, so a
// field named "catalog" of type CatalogClient still counts as a remote call.
func measure(f *File, body *ts.Node, types map[string]string) Cost {
	c := Cost{Complexity: 1}
	var visit func(n *ts.Node, depth int)
	visit = func(n *ts.Node, depth int) {
		t := n.Type(lang)
		if branchTypes[t] {
			c.Complexity++
		}
		if t == "binary_expression" {
			if op := field(n, "operator"); op != nil {
				if s := f.text(op); s == "&&" || s == "||" {
					c.Complexity++
				}
			}
		}
		inner := depth
		if loopTypes[t] && !smallFixedLoop(f, n) {
			inner++
		}
		if t == "method_invocation" {
			name := f.text(field(n, "name"))
			obj := f.text(field(n, "object"))
			if name == "forEach" || streamSteps[name] && strings.Contains(obj, "stream()") {
				inner++
			}
			if i := strings.LastIndex(obj, "."); i >= 0 {
				obj = obj[i+1:]
			}
			declared := simpleType(types[obj])
			io := false
			if (dbReceiver.MatchString(obj) || dbReceiver.MatchString(declared)) && dbMethod.MatchString(name) {
				c.DBCalls++
				io = true
			} else if obj != "" && (remoteObject.MatchString(obj) || remoteObject.MatchString(declared)) {
				c.Remote++
				io = true
			}
			if io && depth > 0 {
				c.LoopIO++
			}
		}
		c.LoopDepth = max(c.LoopDepth, inner)
		for _, k := range named(n) {
			if runsOnce(n, k) {
				visit(k, depth)
			} else {
				visit(k, inner)
			}
		}
	}
	visit(body, 0)
	switch {
	case c.LoopIO > 0:
		c.Estimate = "O(n) round trips"
	case c.LoopDepth >= 3:
		c.Estimate = "O(n³)"
	case c.LoopDepth == 2:
		c.Estimate = "O(n²)"
	case c.LoopDepth == 1:
		c.Estimate = "O(n)"
	default:
		c.Estimate = "O(1)"
	}
	return c
}
