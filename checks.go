package java

import (
	"regexp"
	"strconv"
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
func Check(f *File, lines map[int]bool) []Issue { return CheckWith(f, lines, nil) }

// Facts is what the checks know about the whole project beyond the file they read. Nil means nothing.
type Facts struct {
	// Optional names the project's methods that return an Optional, wherever they are declared. A name some
	// other project method declares with another return type is left out, so a call to it is never guessed.
	Optional map[string]bool
}

// FactsOf gathers Facts from parsed files, usually every main Java file of the project.
func FactsOf(files []*File) *Facts {
	returns := map[string]bool{}
	for _, f := range files {
		for _, t := range f.Types {
			for _, m := range t.Methods {
				opt := strings.HasPrefix(simpleTypeRaw(m.Returns), "Optional")
				if seen, ok := returns[m.Name]; ok {
					opt = opt && seen
				}
				returns[m.Name] = opt
			}
		}
	}
	facts := &Facts{Optional: map[string]bool{}}
	for name, opt := range returns {
		if opt {
			facts.Optional[name] = true
		}
	}
	return facts
}

// CheckWith runs the checks on a file, using facts about the rest of the project when given.
func CheckWith(f *File, lines map[int]bool, facts *Facts) []Issue {
	if f.Root == nil {
		return nil
	}
	if facts == nil {
		facts = &Facts{}
	}
	c := &checker{f: f, lines: lines, test: strings.Contains(f.Path, "/src/test/"), facts: facts}
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
	facts *Facts
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
	secretName = regexp.MustCompile(`(?i)(password|passwd|secret|api_?key|token|credential)s?$`)
	sqlMethods = map[string]bool{"createQuery": true, "createNativeQuery": true, "query": true, "queryForObject": true, "queryForList": true, "update": true, "execute": true, "executeQuery": true, "executeUpdate": true, "prepareStatement": true,
		"sql": true, "queryForMap": true, "queryForRowSet": true, "queryForStream": true, "batchUpdate": true, "addBatch": true, "prepareCall": true} // sql: Spring's JdbcClient and R2DBC's DatabaseClient
	constantName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	boxedTypes   = map[string]bool{"Long": true, "Integer": true, "Short": true, "Byte": true, "Double": true, "Float": true, "Character": true, "Boolean": true, "BigDecimal": true}
	numberLit    = regexp.MustCompile(`^-?[0-9][0-9_]*(\.[0-9_]+)?[dDfF]?$`)
	wholeNumber  = regexp.MustCompile(`^-?[0-9][0-9_]*[lL]?$`)
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
	case obj == "UUID" && name == "randomUUID" && c.inRetryLoop(n):
		c.add(n, "retry-new-id", "high", "A new random id on every retry",
			"Each attempt sends a different id, so the other side cannot tell a retry from a new request. If an earlier attempt "+
				"went through before timing out, the work is done twice, such as a second charge.",
			"Make the id once, before the loop, and send the same one on every attempt.")
	case name == "skip" && args != nil && args.NamedChildCount() == 1 && regexp.MustCompile(`^(java\.lang\.)?(Exception|Throwable|RuntimeException)\.class$`).MatchString(c.f.text(args.NamedChild(0))),
		name == "skipLimit" && args != nil && args.NamedChildCount() == 1 && c.f.text(args.NamedChild(0)) == "Integer.MAX_VALUE":
		c.add(n, "batch-skip-all", "high", "Batch step skips every failure",
			"Any error on any item is skipped and the step still finishes as COMPLETED, so items are lost without the run failing "+
				"or anyone being told.",
			"Skip only the specific exceptions you expect, with a small skipLimit, and record skipped items with a SkipListener.")
	case obj == "Duration" && (name == "ofMillis" || name == "ofSeconds") && args != nil && args.NamedChildCount() == 1:
		c.durationUnit(n, name, args.NamedChild(0))
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
		return strings.HasPrefix(name, "findBy") || name == "findById" || name == "findFirst" || name == "findAny" || c.facts.Optional[name]
	}
	return false
}

func simpleTypeRaw(t string) string {
	if i := strings.LastIndex(t, "."); i >= 0 && !strings.Contains(t[:i], "<") {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// allLiterals says the text is fixed: string literals and constants named in capitals, such as SELECT, joined.
func allLiterals(f *File, n *ts.Node) bool {
	if n.Type(lang) == "string_literal" || n.Type(lang) == "identifier" && constantName.MatchString(f.text(n)) {
		return true
	}
	if n.Type(lang) == "parenthesized_expression" && n.NamedChildCount() == 1 {
		return allLiterals(f, n.NamedChild(0))
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
		if c.isDouble(a, sc) || numberLit.MatchString(text) && !wholeNumber.MatchString(text) {
			c.add(n, "bigdecimal-double", "medium", "BigDecimal made from a double",
				"new BigDecimal(0.1) is 0.1000000000000000055511151231257827: the double's binary error is kept. Money totals drift and equality checks fail.",
				`new BigDecimal("0.1") or BigDecimal.valueOf(0.1)`)
		}
	}
}

// isDouble says an expression works out to a double or float: a floating literal or variable, a cast to one, a Math
// function that returns one, or arithmetic with any of these, such as 1 + percent / 100 with a double percent.
func (c *checker) isDouble(n *ts.Node, sc scope) bool {
	switch n.Type(lang) {
	case "decimal_floating_point_literal":
		return true
	case "identifier":
		t := simpleTypeRaw(sc[c.f.text(n)])
		return t == "double" || t == "float" || t == "Double" || t == "Float"
	case "cast_expression":
		t := c.f.text(field(n, "type"))
		return t == "double" || t == "float"
	case "parenthesized_expression":
		return n.NamedChildCount() == 1 && c.isDouble(n.NamedChild(0), sc)
	case "method_invocation":
		name := c.f.text(field(n, "name"))
		return c.f.text(field(n, "object")) == "Math" && (name == "pow" || name == "sqrt" || name == "random" || name == "exp" || name == "log")
	case "binary_expression":
		switch c.f.text(field(n, "operator")) {
		case "+", "-", "*", "/", "%":
			return c.isDouble(field(n, "left"), sc) || c.isDouble(field(n, "right"), sc)
		}
	}
	return false
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
	c.unusedPathVariable(n)
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
			from, to := m.Cost.LoopFrom, m.Cost.LoopTo
			if from == 0 {
				from, to = m.Line, m.End
			}
			c.addAt(from, to, "slow-call-in-loop", "medium", "Database or remote call repeated in a loop",
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

// durationUnit finds a time written in one unit and passed as another: Duration.ofMillis(24 * 60 * 60) is 86.4
// seconds, not a day, and Duration.ofSeconds(3_600_000) is 41 days, not an hour.
func (c *checker) durationUnit(n *ts.Node, name string, arg *ts.Node) {
	v, ok := constant(c.f, arg)
	if !ok || v <= 0 {
		return
	}
	switch {
	case name == "ofMillis" && v%3600 == 0 && v%1000 != 0:
		c.add(n, "duration-unit", "high", "Seconds passed as milliseconds",
			"Duration.ofMillis("+c.f.text(arg)+") is "+itoa(int(v/1000))+"."+itoa(int(v%1000/100))+" seconds. The number is a whole count of hours in seconds, so it was most likely meant as seconds.",
			"Duration.ofSeconds("+c.f.text(arg)+"), or better Duration.ofHours(...) / ofMinutes(...).")
	case name == "ofSeconds" && v%3_600_000 == 0:
		c.add(n, "duration-unit", "high", "Milliseconds passed as seconds",
			"Duration.ofSeconds("+c.f.text(arg)+") is "+itoa(int(v/86400))+" days. The number is a whole count of hours in milliseconds, so it was most likely meant as milliseconds.",
			"Duration.ofMillis("+c.f.text(arg)+"), or better Duration.ofHours(...) / ofMinutes(...).")
	}
}

// constant works out a whole-number expression made only of literals and *, such as 24 * 60 * 60.
func constant(f *File, n *ts.Node) (int64, bool) {
	switch n.Type(lang) {
	case "decimal_integer_literal":
		v, err := strconv.ParseInt(strings.TrimRight(strings.ReplaceAll(f.text(n), "_", ""), "lL"), 10, 64)
		return v, err == nil
	case "parenthesized_expression":
		if n.NamedChildCount() == 1 {
			return constant(f, n.NamedChild(0))
		}
	case "binary_expression":
		if op := field(n, "operator"); op != nil && f.text(op) == "*" {
			a, ok1 := constant(f, field(n, "left"))
			b, ok2 := constant(f, field(n, "right"))
			return a * b, ok1 && ok2
		}
	}
	return 0, false
}

// unusedPathVariable finds a value the URL names that the method never reads, such as a customerId in
// /customers/{customerId}/orders/{orderId}: the request is served whatever it says, so when it names an owner, any
// caller can reach another owner's data.
func (c *checker) unusedPathVariable(n *ts.Node) {
	ps, body := field(n, "parameters"), field(n, "body")
	if ps == nil || body == nil {
		return
	}
	text := c.f.text(body)
	for _, p := range named(ps) {
		if p.Type(lang) != "formal_parameter" || !strings.Contains(c.f.text(p), "@PathVariable") {
			continue
		}
		name := c.f.text(field(p, "name"))
		if name == "" || regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b`).MatchString(text) {
			continue
		}
		c.add(n, "unused-path-variable", "high", "The URL's "+name+" is never used",
			"The request is served whatever "+name+" says. When it names an owner, such as a customer or account, any caller "+
				"can read or change another owner's data by changing the other id in the URL.",
			"Check that the record belongs to "+name+", for example .filter(o -> o.customerId() == "+name+"), and answer 404 when it does not.")
	}
}

// inRetryLoop says n is inside a loop that catches errors and goes round again: a retry.
func (c *checker) inRetryLoop(n *ts.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type(lang) {
		case "method_declaration", "lambda_expression", "class_body":
			return false
		case "for_statement", "while_statement", "do_statement", "enhanced_for_statement":
			return strings.Contains(c.f.text(p), "catch")
		}
	}
	return false
}
