package java

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// Issue is something a check found, with what to do about it.
type Issue struct {
	Rule       string
	Severity   string // critical, high, medium, low, info
	Title      string
	Detail     string
	Suggestion string
	Line, End  int
}

// Check runs every check on a parsed file and returns what it finds. lines limits results to
// those lines (the lines a commit changed); nil means the whole file.
func Check(f *File, lines map[int]bool) []Issue {
	if f.Root == nil {
		return nil
	}
	c := &checker{f: f, lines: lines, test: strings.Contains(f.Path, "/src/test/")}
	c.walk(f.Root, scope{})
	for _, t := range f.Types {
		c.typeChecks(t)
	}
	return c.out
}

type checker struct {
	f     *File
	lines map[int]bool
	test  bool
	out   []Issue
}

// scope maps names visible in a method to their declared types.
type scope map[string]string

func (c *checker) add(n *ts.Node, rule, severity, title, detail, suggestion string) {
	start, end := line(n), endLine(n)
	if c.lines != nil {
		hit := false
		for l := start; l <= end; l++ {
			hit = hit || c.lines[l]
		}
		if !hit {
			return
		}
	}
	for _, o := range c.out { // one finding per rule per line
		if o.Rule == rule && o.Line == start {
			return
		}
	}
	c.out = append(c.out, Issue{Rule: rule, Severity: severity, Title: title, Detail: detail, Suggestion: suggestion, Line: start, End: end})
}

var (
	secretName  = regexp.MustCompile(`(?i)(password|passwd|secret|api_?key|token|credential)s?$`)
	sqlMethods  = map[string]bool{"createQuery": true, "createNativeQuery": true, "query": true, "queryForObject": true, "queryForList": true, "update": true, "execute": true, "executeQuery": true, "executeUpdate": true, "prepareStatement": true}
	boxedTypes  = map[string]bool{"Long": true, "Integer": true, "Short": true, "Byte": true, "Double": true, "Float": true, "Character": true, "Boolean": true, "BigDecimal": true}
	numberLit   = regexp.MustCompile(`^-?[0-9][0-9_]*(\.[0-9_]+)?[dDfF]?$`)
	wholeNumber = regexp.MustCompile(`^-?[0-9][0-9_]*[lL]?$`)
)

func (c *checker) walk(n *ts.Node, sc scope) {
	switch n.Type(lang) {
	case "class_body", "enum_body", "interface_body":
		sc = scope{}
		for _, m := range named(n) {
			if m.Type(lang) == "field_declaration" {
				c.declare(m, sc)
			}
		}
	case "method_declaration", "constructor_declaration", "lambda_expression":
		inner := scope{}
		for k, v := range sc {
			inner[k] = v
		}
		if ps := field(n, "parameters"); ps != nil {
			for _, p := range named(ps) {
				if t := field(p, "type"); t != nil {
					inner[c.f.text(field(p, "name"))] = c.f.text(t)
				}
			}
		}
		sc = inner
		if n.Type(lang) != "lambda_expression" {
			c.methodChecks(n)
		}
	case "local_variable_declaration":
		c.declare(n, sc)
	case "catch_clause":
		c.emptyCatch(n)
	case "method_invocation":
		c.call(n, sc)
	case "binary_expression":
		c.equality(n, sc)
	case "object_creation_expression":
		c.creation(n, sc)
	case "variable_declarator":
		c.secret(n)
	}
	for _, k := range named(n) {
		c.walk(k, sc)
	}
}

func (c *checker) declare(n *ts.Node, sc scope) {
	typ := c.f.text(field(n, "type"))
	for _, d := range named(n) {
		if d.Type(lang) == "variable_declarator" {
			sc[c.f.text(field(d, "name"))] = typ
		}
	}
}

func (c *checker) emptyCatch(n *ts.Node) {
	body := field(n, "body")
	if body == nil || body.NamedChildCount() > 0 {
		return
	}
	// A comment in the block says the developer meant it.
	if strings.Contains(c.f.text(body), "//") || strings.Contains(c.f.text(body), "/*") {
		return
	}
	c.add(n, "empty-catch", "medium", "Exception swallowed by an empty catch block",
		"The error disappears: the caller carries on as if it worked, and nothing is logged to find it later.",
		"Log it with context, rethrow it wrapped, or handle it. If ignoring it is right, say why in a comment.")
}

func (c *checker) call(n *ts.Node, sc scope) {
	name := c.f.text(field(n, "name"))
	obj := c.f.text(field(n, "object"))
	args := field(n, "arguments")
	switch {
	case name == "printStackTrace" && (args == nil || args.NamedChildCount() == 0) && !c.test:
		c.add(n, "print-stack-trace", "low", "printStackTrace() instead of the logger",
			"It writes to standard error with no level, time or request context, so it is lost in production logs.",
			`log.error("What failed and for which input", e);`)
	case (obj == "System.out" || obj == "System.err") && strings.HasPrefix(name, "print") && !c.test:
		c.add(n, "system-out", "low", "Printing to the console instead of logging",
			"Console output has no level and cannot be turned off or searched like log lines.",
			"Use the class's logger.")
	case name == "get" && (args == nil || args.NamedChildCount() == 0) && c.optional(field(n, "object"), sc):
		if method := enclosing(n, "method_declaration"); method == nil || !regexp.MustCompile(`\.(isPresent|isEmpty|ifPresent)\(`).MatchString(c.f.text(method)) {
			c.add(n, "optional-get", "high", "Optional.get() without checking it has a value",
				"When nothing is found this throws NoSuchElementException, which reaches users as a 500 error instead of a clear 404 or 400.",
				`.orElseThrow(() -> new NotFoundException("No order " + id))`)
		}
	case sqlMethods[name] && args != nil && args.NamedChildCount() > 0:
		if first := args.NamedChild(0); first.Type(lang) == "binary_expression" && strings.Contains(c.f.text(first), "+") && strings.Contains(c.f.text(first), `"`) {
			if !allLiterals(c.f, first) {
				c.add(n, "sql-concatenation", "critical", "SQL built by joining strings",
					"Values joined into the query text can change the query itself: SQL injection. They also stop the database reusing the query plan.",
					"Use a parameter: `where id = :id` with setParameter, or `?` with arguments.")
			}
		}
	}
}

// optional says whether an expression is an Optional: a variable declared as one, or a call to a
// find... method, which Spring Data repositories return as Optional.
func (c *checker) optional(n *ts.Node, sc scope) bool {
	if n == nil {
		return false
	}
	switch n.Type(lang) {
	case "identifier":
		return strings.HasPrefix(simpleTypeRaw(sc[c.f.text(n)]), "Optional")
	case "method_invocation":
		name := c.f.text(field(n, "name"))
		return strings.HasPrefix(name, "findBy") || name == "findById" || name == "findFirst" || name == "findAny"
	}
	return false
}

func simpleTypeRaw(t string) string {
	if i := strings.LastIndex(t, "."); i >= 0 && !strings.Contains(t[:i], "<") {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

func allLiterals(f *File, n *ts.Node) bool {
	if n.Type(lang) == "string_literal" {
		return true
	}
	if n.Type(lang) != "binary_expression" {
		return false
	}
	for _, k := range named(n) {
		if !allLiterals(f, k) {
			return false
		}
	}
	return true
}

func enclosing(n *ts.Node, kind string) *ts.Node {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type(lang) == kind {
			return p
		}
	}
	return nil
}

// equality finds == and != between objects, which compares identity rather than value.
func (c *checker) equality(n *ts.Node, sc scope) {
	op := c.f.text(field(n, "operator"))
	if op != "==" && op != "!=" {
		return
	}
	left, right := field(n, "left"), field(n, "right")
	if left == nil || right == nil || c.f.text(left) == "null" || c.f.text(right) == "null" {
		return
	}
	isString := func(x *ts.Node) bool {
		return x.Type(lang) == "string_literal" || x.Type(lang) == "identifier" && simpleTypeRaw(sc[c.f.text(x)]) == "String"
	}
	isBoxed := func(x *ts.Node) bool {
		return x.Type(lang) == "identifier" && boxedTypes[simpleTypeRaw(sc[c.f.text(x)])]
	}
	// Anything compared with == against a known String is compared by identity, unless it is a
	// primitive literal, which would not compile anyway.
	plain := func(x *ts.Node) bool {
		k := x.Type(lang)
		return k == "character_literal" || k == "decimal_integer_literal" || k == "true" || k == "false"
	}
	switch {
	case isString(left) && !plain(right) || isString(right) && !plain(left):
		c.add(n, "string-identity", "high", "Strings compared with "+op,
			op+" checks whether both are the same object, not the same text. Equal strings from a request or the database are usually different objects, so this is false when it should be true.",
			"Use a.equals(b), or Objects.equals(a, b) when either can be null.")
	case isBoxed(left) && isBoxed(right):
		c.add(n, "boxed-identity", "high", "Boxed numbers compared with "+op,
			op+" on Long or Integer compares objects. It works for small values from a cache, so tests pass, then fails for larger ids.",
			"Use Objects.equals(a, b), or compare primitive values.")
	}
}

func (c *checker) creation(n *ts.Node, sc scope) {
	typ := c.f.text(field(n, "type"))
	args := field(n, "arguments")
	if typ == "BigDecimal" && args != nil && args.NamedChildCount() == 1 {
		a := args.NamedChild(0)
		text := c.f.text(a)
		isDouble := a.Type(lang) == "decimal_floating_point_literal" ||
			a.Type(lang) == "identifier" && (sc[text] == "double" || sc[text] == "float" || sc[text] == "Double")
		if isDouble || numberLit.MatchString(text) && !wholeNumber.MatchString(text) {
			c.add(n, "bigdecimal-double", "medium", "BigDecimal made from a double",
				"new BigDecimal(0.1) is 0.1000000000000000055511151231257827: the double's binary error is kept. Money totals drift and equality checks fail.",
				`new BigDecimal("0.1") or BigDecimal.valueOf(0.1)`)
		}
	}
}

func (c *checker) secret(n *ts.Node) {
	name := c.f.text(field(n, "name"))
	v := field(n, "value")
	if v == nil || v.Type(lang) != "string_literal" || c.test || !secretName.MatchString(name) {
		return
	}
	if text := strings.Trim(c.f.text(v), `"`); len(text) >= 4 && !strings.HasPrefix(text, "${") {
		c.add(n, "hardcoded-secret", "high", "A secret written in the code",
			"Anyone who can read the repository, its history or a built jar can read it, and changing it needs a release.",
			"Read it from configuration or a secret store: @Value(\"${payment.api-key}\").")
	}
}

func (c *checker) methodChecks(n *ts.Node) {
	mods := ""
	for _, k := range named(n) {
		if k.Type(lang) == "modifiers" {
			mods = c.f.text(k)
		}
	}
	if strings.Contains(mods, "@Transactional") && regexp.MustCompile(`\bprivate\b`).MatchString(mods) {
		c.add(n, "transactional-private", "high", "@Transactional on a private method does nothing",
			"Spring applies transactions through a proxy, which only sees public methods called from another bean. The code runs without a transaction.",
			"Make the method public and call it from another bean, or put @Transactional on the public caller.")
	}
	if strings.Contains(mods, "@Transactional") && n.Type(lang) == "method_declaration" {
		if body := field(n, "body"); body != nil && regexp.MustCompile(`catch\s*\(\s*(Exception|RuntimeException|Throwable)\b`).MatchString(c.f.text(body)) &&
			!regexp.MustCompile(`\bthrow\b`).MatchString(c.f.text(body)) {
			c.add(n, "transaction-swallowed", "high", "@Transactional method catches the error and carries on",
				"Catching the exception stops the rollback, so half the work is committed.",
				"Rethrow after logging, or call TransactionAspectSupport.currentTransactionStatus().setRollbackOnly().")
		}
	}
}

func (c *checker) typeChecks(t Type) {
	if c.test {
		return
	}
	var equals, hash *Method
	for i := range t.Methods {
		m := &t.Methods[i]
		switch {
		case m.Name == "equals" && len(m.Params) == 1:
			equals = m
		case m.Name == "hashCode" && len(m.Params) == 0:
			hash = m
		}
		if m.Cost.LoopIO > 0 {
			c.addAt(m.Line, m.End, "slow-call-in-loop", "medium", "Database or remote call repeated in a loop",
				"Each pass makes a round trip, so time grows with the data: the N+1 problem. It is fast with test data and slow in production.",
				"Load everything in one query (findAllById, a join or an IN clause) or one batched call, then loop in memory.")
		}
		if m.Cost.Complexity > 15 {
			c.addAt(m.Line, m.End, "complex-method", "low", m.Name+"() has "+itoa(m.Cost.Complexity)+" paths",
				"Each path needs its own test, and changes in one branch easily break another.",
				"Split it into smaller methods, each with one job.")
		}
	}
	if equals != nil && hash == nil && t.Kind == "class" {
		c.addAt(equals.Line, equals.End, "equals-without-hashcode", "medium", "equals() without hashCode()",
			"Equal objects get different hash codes, so HashMap and HashSet treat them as different: lookups miss and sets hold duplicates.",
			"Add hashCode() using the same fields: Objects.hash(a, b).")
	}
	for _, f := range t.Fields {
		if strings.Contains(f.Type, "SimpleDateFormat") {
			if decl := c.fieldDeclaration(f.Line); strings.Contains(decl, "static") {
				c.addAt(f.Line, f.Line, "shared-date-format", "high", "Shared SimpleDateFormat is not thread-safe",
					"Concurrent requests corrupt its internal state and produce wrong dates or exceptions, only under load.",
					"Use java.time.format.DateTimeFormatter, which is immutable and safe to share.")
			}
		}
	}
}

func (c *checker) fieldDeclaration(line int) string {
	if line < 1 || line > len(c.f.Lines) {
		return ""
	}
	return c.f.Lines[line-1]
}

// addAt is add for findings known by line numbers rather than a node.
func (c *checker) addAt(start, end int, rule, severity, title, detail, suggestion string) {
	if c.lines != nil {
		hit := false
		for l := start; l <= end; l++ {
			hit = hit || c.lines[l]
		}
		if !hit {
			return
		}
	}
	c.out = append(c.out, Issue{Rule: rule, Severity: severity, Title: title, Detail: detail, Suggestion: suggestion, Line: start, End: end})
}
