package java

import (
	"strings"
	"testing"

	ts "github.com/odvcencio/gotreesitter"
)

func parseAll(srcs map[string]string) []*File {
	var out []*File
	for p, s := range srcs {
		out = append(out, Parse(p, []byte(s)))
	}
	return out
}

var habitProject = map[string]string{
	"Emails.java": `class Emails { static String normal(String e) { return e.toLowerCase(); } }`,
	"Paging.java": `record Paging(int page, int size) { static Paging of(Integer p, Integer s) { return new Paging(p == null ? 1 : p, s == null ? 20 : s); } }`,
	"A.java": `class A {
    View a(Employee e) { return new View(Emails.normal(e.email())); }
    View b(Employee e) { return new View(Emails.normal(e.email())); }
    View c(Employee e) { return new View(Emails.normal(e.email())); }
    String v(View v) { return v.email(); }
    List<E> p1(Integer p, Integer s) { return repo.page(Paging.of(p, s)); }
    List<E> p2(Integer p, Integer s) { return repo.page(Paging.of(p, s)); }
    List<E> p3(Integer p, Integer s) { return repo.page(Paging.of(p, s)); }
}`,
	"Repo.java": `class Repo {
    private static final String SELECT = "SELECT id, email FROM employee";
    List<E> page() { return jdbc.sql(SELECT + " WHERE deleted_at IS NULL LIMIT :n").query(this::row).list(); }
    E find(long id) { return jdbc.sql(SELECT + " WHERE id = :id AND deleted_at IS NULL").param("id", id).query(this::row).single(); }
    List<E> all() { return jdbc.sql(SELECT + " WHERE deleted_at IS NULL ORDER BY id").query(this::row).list(); }
    List<D> depts() { return jdbc.sql("SELECT id FROM department").query(D.class).list(); }
}`,
}

func TestMineHabits(t *testing.T) {
	h := MineHabits(parseAll(habitProject))
	// Not yet habits: nothing breaks them, so a rule with no exception is not reported as a habit to break.
	if len(h.Wraps)+len(h.Factories)+len(h.Filters) != 0 {
		t.Fatalf("habits never broken are not reported: %+v %+v %+v", h.Wraps, h.Factories, h.Filters)
	}

	broken := map[string]string{}
	for k, v := range habitProject {
		broken[k] = v
	}
	broken["B.java"] = `class B {
    String csv(Employee e) { return e.email(); }
    List<E> g(Integer p, Integer s) { return repo.page(new Paging(p, s)); }
}`
	broken["Repo2.java"] = `class Repo2 {
    List<E> everyone() { return jdbc.sql("SELECT id, email FROM employee ORDER BY id").query(this::row).list(); }
}`
	files := parseAll(broken)
	h = MineHabits(files)
	if w := h.Wraps["Employee.email"]; w == nil || w.Usual != "Emails.normal" || w.Kept != 3 || w.All != 4 {
		t.Errorf("wrap habit: %+v", w)
	}
	if h.Wraps["View.email"] != nil {
		t.Error("a view's email() is another accessor, never wrapped: no habit")
	}
	if x := h.Factories["Paging"]; x == nil || x.Usual != "Paging.of" || x.Kept != 3 || x.All != 4 {
		t.Errorf("factory habit: %+v", x)
	}
	if x := h.Filters["employee"]; x == nil || x.Usual != "deleted_at is null" || x.Kept != 3 || x.All != 4 {
		t.Errorf("filter habit: %+v", x)
	}
	if h.Filters["department"] != nil {
		t.Error("one query on a table is no habit")
	}

	facts := FactsOf(files)
	var b, r2 *File
	for _, f := range files {
		switch f.Path {
		case "B.java":
			b = f
		case "Repo2.java":
			r2 = f
		}
	}
	rules := map[string]int{}
	for _, f := range []*File{b, r2} {
		for _, is := range CheckWith(f, nil, facts) {
			rules[is.Rule]++
		}
	}
	if rules["habit-wrap"] != 1 || rules["habit-factory"] != 1 || rules["habit-filter"] != 1 {
		t.Errorf("each broken habit reported once: %v", rules)
	}
	// The type that owns the factory may use its own constructor, and code keeping the habits is quiet.
	for _, f := range files {
		if f.Path == "B.java" || f.Path == "Repo2.java" {
			continue
		}
		if got := CheckWith(f, nil, facts); len(got) != 0 {
			t.Errorf("%s keeps the habits but got %+v", f.Path, got)
		}
	}
}

// A lambda parameter has no written type, so it is never matched to another method's variable of the same name.
func TestAccessorTypes(t *testing.T) {
	f := Parse("A.java", []byte(`class A {
    void a(View e) { use(e.email()); }
    void b() { list.forEach(e -> use(e.email())); }
    void c(List<Employee> all) { for (Employee e : all) { use(e.email()); } }
}`))
	var keys []string
	walkAll(f.Root, func(n *ts.Node) {
		if n.Type(lang) == "method_invocation" {
			if key, _, ok := accessorUse(f, n); ok {
				keys = append(keys, key)
			}
		}
	})
	if strings.Join(keys, ",") != "View.email,Employee.email" {
		t.Errorf("got %v", keys)
	}
}
