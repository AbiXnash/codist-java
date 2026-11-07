package java

import (
	"sort"
	"strings"
	"testing"
)

// Each case is a snippet with the rules it must raise. The "clean" cases must raise nothing.
var checkCases = []struct {
	name, src string
	want      []string
}{
	{"empty catch", `class A { void a() { try { x(); } catch (Exception e) { } } }`, []string{"empty-catch"}},
	{"catch with a reason", `class A { void a() { try { x(); } catch (Exception e) { // not needed here
	} } }`, nil},
	{"printStackTrace", `class A { void a() { try { x(); } catch (Exception e) { e.printStackTrace(); } } }`, []string{"print-stack-trace"}},
	{"console", `class A { void a() { System.out.println("hi"); } }`, []string{"system-out"}},
	{"optional get on find", `class A { Order a(long id) { return repo.findById(id).get(); } }`, []string{"optional-get"}},
	{"optional get after check", `class A { Order a(long id) { Optional<Order> o = repo.findById(id); if (o.isPresent()) { return o.get(); } return null; } }`, nil},
	{"map get is fine", `class A { String a(Map<String,String> m) { return m.get("k"); } }`, nil},
	{"string ==", `class A { boolean a(String s) { return s == "OPEN"; } }`, []string{"string-identity"}},
	{"string == a getter", `class A { boolean a(Order o, String s) { return o.getStatus() == s; } }`, []string{"string-identity"}},
	{"string null check is fine", `class A { boolean a(String s) { return s == null; } }`, nil},
	{"boxed ==", `class A { boolean a(Long x, Long y) { return x == y; } }`, []string{"boxed-identity"}},
	{"primitive == is fine", `class A { boolean a(long x, long y) { return x == y; } }`, nil},
	{"BigDecimal from double", `class A { BigDecimal a() { return new BigDecimal(0.1); } }`, []string{"bigdecimal-double"}},
	{"BigDecimal from string is fine", `class A { BigDecimal a() { return new BigDecimal("0.1"); } }`, nil},
	{"BigDecimal from whole number is fine", `class A { BigDecimal a() { return new BigDecimal(10); } }`, nil},
	{"secret", `class A { private static final String API_KEY = "sk-live-1234"; }`, []string{"hardcoded-secret"}},
	{"secret placeholder is fine", `class A { @Value("${x}") String password = "${db.password}"; }`, nil},
	{"SQL joined", `class A { List a(String name) { return em.createQuery("select o from Order o where o.name = '" + name + "'").getResultList(); } }`, []string{"sql-concatenation"}},
	{"SQL from literals only is fine", `class A { List a() { return em.createQuery("select o " + "from Order o").getResultList(); } }`, nil},
	{"private transactional", `class A { @Transactional private void a() { } }`, []string{"transactional-private"}},
	{"transaction swallowed", `class A { @Transactional public void a() { try { x(); } catch (Exception e) { log.warn("x", e); } } }`, []string{"transaction-swallowed"}},
	{"transaction rethrown is fine", `class A { @Transactional public void a() { try { x(); } catch (Exception e) { throw new IllegalStateException(e); } } }`, nil},
	{"equals without hashCode", `class A { public boolean equals(Object o) { return true; } }`, []string{"equals-without-hashcode"}},
	{"equals with hashCode is fine", `class A { public boolean equals(Object o) { return true; } public int hashCode() { return 1; } }`, nil},
	{"shared date format", `class A { private static final SimpleDateFormat F = new SimpleDateFormat("yyyy"); }`, []string{"shared-date-format"}},
	{"N+1", `class A { OrderRepository orders; void a(List<Long> ids) { for (Long id : ids) { orders.findById(id); } } }`, []string{"slow-call-in-loop"}},
}

func TestChecks(t *testing.T) {
	for _, c := range checkCases {
		f := Parse("src/main/java/A.java", []byte(c.src))
		var got []string
		for _, i := range Check(f, nil) {
			got = append(got, i.Rule)
		}
		sort.Strings(got)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v, want %v", c.name, got, want)
		}
	}
}

func TestChecksOnlyChangedLines(t *testing.T) {
	src := "class A {\n  void a() {\n    System.out.println(1);\n    System.out.println(2);\n  }\n}\n"
	f := Parse("src/main/java/A.java", []byte(src))
	got := Check(f, map[int]bool{4: true})
	if len(got) != 1 || got[0].Line != 4 {
		t.Errorf("only line 4 changed: %+v", got)
	}
	if tests := Check(Parse("svc/src/test/java/ATest.java", []byte(src)), nil); len(tests) != 0 {
		t.Errorf("console output in tests is fine: %+v", tests)
	}
}
