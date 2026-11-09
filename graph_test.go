package java

import (
	"slices"
	"strings"
	"testing"

	"github.com/abxengg/codist-git/testrepo"
)

func shop(t *testing.T) *Index {
	t.Helper()
	src, other := testrepo.Sources()
	ix := Build(src, other)
	if len(ix.Types) != 8 {
		t.Fatalf("%d types", len(ix.Types))
	}
	return ix
}

// call is "Class.method->Class.method", with "?" appended when the receiver's type was not known.
func calls(cg *CallGraph) []string {
	var out []string
	for _, c := range cg.Calls() {
		s := c.From.Class + "." + c.From.Name + "->" + c.To.Class + "." + c.To.Name
		if !c.Confirmed {
			s += "?"
		}
		out = append(out, s)
	}
	return out
}

func TestCallGraphAcrossServices(t *testing.T) {
	ix := shop(t)
	got := calls(BuildCallGraph(ix.Sources, ix.Types))
	for _, want := range []string{
		"OrderController.get->OrderService.find",          // through a field
		"OrderService.payAll->PaymentClient.charge",       // through an injected Feign client
		"PaymentClient.charge->PaymentController.charge",  // Feign: the client reaches the other service's handler
		"PaymentController.charge->PaymentService.charge", // and on into that service
		"OrderService.discount->Order.applyDiscount",      // through a parameter
		"OrderService.payAll->Order.getId",                // through a for-each variable
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, strings.Join(got, "\n"))
		}
	}
	if slices.Contains(got, "OrderService.find->OrderRepository.findById") {
		t.Error("a Spring Data method the project does not declare is not a project call")
	}
}

func TestCallGraphRoutes(t *testing.T) {
	ix := shop(t)
	cg := BuildCallGraph(ix.Sources, ix.Types)
	for _, c := range cg.Calls() {
		if c.To.Class == "PaymentController" && c.To.Name == "charge" {
			if !slices.Equal(c.To.Routes, []string{"POST /payments/{id}"}) || c.To.Test || c.To.Signature == "" {
				t.Errorf("%+v", c.To)
			}
			return
		}
	}
	t.Error("PaymentController.charge is never called")
}

var shapes = map[string][]byte{
	"app/src/main/java/com/x/Shape.java":  []byte("package com.x;\npublic interface Shape { double area(); }\n"),
	"app/src/main/java/com/x/Circle.java": []byte("package com.x;\npublic class Circle implements Shape {\n  public double area() { return 1; }\n}\n"),
	"app/src/main/java/com/x/Point.java":  []byte("package com.x;\npublic record Point(Shape shape) {\n  double size() { return shape.area(); }\n}\n"),
	"app/src/main/java/com/x/Util.java": []byte(`package com.x;
public class Util {
  static int twice(int x) { return x * 2; }
  int one() { return 1; }

  int one(int a) {
    return a;
  }
}
`),
	"app/src/main/java/com/x/Main.java": []byte(`package com.x;
public class Main extends com.x.Base<String> {
  private Shape shape;
  private Shape[] many;
  void run(java.util.List<Shape> all, Shape[] arr) {
    for (Shape s : all) { s.area(); }
    Circle c = new Circle();
    c.area();
    Util.twice(2);
    this.shape.area();
    build().unique();
    helper();
    java.util.List.of().size();
  }
  void helper() {}
  Main build() { return this; }
  void unique() {}
}
`),
	"app/src/main/java/com/x/Base.java":     []byte("package com.x;\npublic abstract class Base<T> { void helper() {} }\n"),
	"app/src/test/java/com/x/MainTest.java": []byte("package com.x;\nclass MainTest { void t() { new Main().helper(); } }\n"),
	"app/src/main/java/com/x/Broken.java":   []byte(""),
}

func TestCallGraphKinds(t *testing.T) {
	got := calls(BuildCallGraph(shapes, nil))
	for _, want := range []string{
		"Main.run->Shape.area",   // a for-each variable
		"Main.run->Circle.area",  // a call through the interface can reach the implementation
		"Main.run->Util.twice",   // a static call
		"Main.run->Main.helper",  // an unqualified call
		"Main.run->Base.helper",  // and the same method up the hierarchy
		"Main.run->Main.unique?", // a chained call, matched by its unique name
		"Point.size->Shape.area", // a record component
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, strings.Join(got, "\n"))
		}
	}
	for _, c := range BuildCallGraph(shapes, nil).Calls() {
		if c.From.Name == "t" && !c.From.Test {
			t.Error("a test file's method is a test")
		}
		if c.To.Class == "Util" && c.To.Name == "one" && c.To.EndLine != 8 {
			t.Errorf("overloads share an entry that ends with the last one: %d", c.To.EndLine)
		}
	}
	if moduleOfFile("x/y.java") != "" || !isTestFile("a/BTests.java") || isTestFile("a/B.java") {
		t.Error("module or test file")
	}
}

func TestShopModules(t *testing.T) {
	ix := shop(t)
	pay, order := ix.Module("payment-service"), ix.Module("order-service")
	if pay == nil || order == nil || ix.Module("nope") != nil {
		t.Fatal("modules")
	}
	if pay.App != "payment-service" || pay.Port != "8082" || order.App != "order-service" || order.Port != "8081" {
		t.Errorf("config: %+v %+v", pay, order)
	}
	if !slices.Equal(order.Calls, []string{"payment-service"}) || !slices.Equal(pay.CalledBy, []string{"order-service"}) {
		t.Errorf("calls %v, called by %v", order.Calls, pay.CalledBy)
	}
	roles := map[string]string{}
	for _, ty := range ix.Types {
		roles[ty.Name] = ty.Stereotype
	}
	want := map[string]string{"OrderController": "controller", "OrderService": "service", "OrderRepository": "repository",
		"PaymentClient": "client", "OrderApplication": "config", "Order": "other"}
	for name, role := range want {
		if roles[name] != role {
			t.Errorf("%s is %s, want %s", name, roles[name], role)
		}
	}
	client := ix.ByID["com.shop.order.PaymentClient"]
	if client.Feign != "payment-service" || len(client.Endpoints) != 1 || client.Endpoints[0].Path != "/payments/{orderId}" {
		t.Errorf("client %+v", client)
	}
}

func TestResolveAndLookup(t *testing.T) {
	ix := shop(t)
	svc := ix.ByID["com.shop.order.OrderService"]
	repo := ix.ByID["com.shop.order.OrderRepository"]
	cases := map[string]string{
		"Order":          "com.shop.order.Order",
		"PaymentService": "com.shop.payment.PaymentService", // the only one
		"List":           "",                                // imported from outside the project
		"Nope":           "",
		"":               "",
	}
	for name, want := range cases {
		if got := ix.Resolve(name, svc); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", name, got, want)
		}
	}
	if ix.Resolve("JpaRepository", repo) != "" {
		t.Error("a library type resolves to nothing")
	}
	if l := ix.Lookup("Order", svc); l == nil || l.ID != "com.shop.order.Order" {
		t.Error("lookup Order")
	}
	if ix.Lookup("Nope", svc) != nil {
		t.Error("lookup of an unknown type")
	}

	// Two classes named Util: the one in the same package, then an imported one, then a star
	// import, then the one in the same module.
	two := Build(map[string][]byte{
		"a/src/main/java/p/Util.java":   []byte("package p;\npublic class Util {}\n"),
		"b/src/main/java/q/Util.java":   []byte("package q;\npublic class Util { public static class Inner {} }\n"),
		"a/src/main/java/p/User.java":   []byte("package p;\npublic class User {}\n"),
		"c/src/main/java/r/Imp.java":    []byte("package r;\nimport q.Util;\npublic class Imp {}\n"),
		"c/src/main/java/r/Star.java":   []byte("package r;\nimport q.*;\npublic class Star {}\n"),
		"a/src/main/java/s/Other.java":  []byte("package s;\npublic class Other {}\n"),
		"d/src/main/java/t/Alone.java":  []byte("package t;\npublic class Alone {}\n"),
		"b/src/main/java/q/Nested.java": []byte("package q;\npublic class Nested {}\n"),
	}, map[string][]byte{"a/pom.xml": nil, "b/pom.xml": nil, "c/pom.xml": nil, "d/pom.xml": nil})
	from := func(id string) *Type { return two.ByID[id] }
	for _, c := range []struct{ name, from, want string }{
		{"Util", "p.User", "p.Util"},  // same package
		{"Util", "r.Imp", "q.Util"},   // imported
		{"Util", "r.Star", "q.Util"},  // star import
		{"Util", "s.Other", "p.Util"}, // same module
		{"Util", "t.Alone", ""},       // no way to tell
		{"Inner", "q.Nested", "q.Util.Inner"},
	} {
		if got := two.Resolve(c.name, from(c.from)); got != c.want {
			t.Errorf("Resolve(%s from %s) = %q, want %q", c.name, c.from, got, c.want)
		}
	}
	for _, c := range []struct{ from, want string }{{"p.User", "p.Util"}, {"r.Imp", "q.Util"}, {"r.Star", "q.Util"}, {"s.Other", "p.Util"}, {"t.Alone", ""}} {
		got := two.Lookup("Util", from(c.from))
		if (got == nil && c.want != "") || (got != nil && got.ID != c.want) {
			t.Errorf("Lookup(Util from %s) = %v, want %q", c.from, got, c.want)
		}
	}
	if two.Lookup("Util", nil) != nil {
		t.Error("an ambiguous name with no context")
	}
}

func TestStereotypes(t *testing.T) {
	cases := []struct{ path, src, want string }{
		{"a/src/main/java/A.java", "@RestControllerAdvice class A {}", "advice"},
		{"a/src/main/java/A.java", "@Entity class A {}", "entity"},
		{"a/src/main/java/A.java", "@Component class A {}", "component"},
		{"a/src/main/java/A.java", "@Repository class A {}", "repository"},
		{"a/src/main/java/A.java", "@Controller class A {}", "controller"},
		{"a/src/main/java/A.java", "@Deprecated class A {}", "other"},
		{"a/src/main/java/A.java", "class OrderDto {}", "dto"},
		{"a/src/main/java/A.java", "class OrderResponse {}", "dto"},
		{"a/src/main/java/A.java", "class NotFoundException {}", "exception"},
		{"a/src/test/java/A.java", "class A {}", "test"},
		{"a/src/main/java/A.java", "class A { @Test void t() {} }", "test"},
		{"a/src/main/java/A.java", "interface A extends CrudRepository<X, Long> {}", "repository"},
		{"a/src/main/java/A.java", "interface A extends Comparable<A> {}", "other"},
	}
	for _, c := range cases {
		f := Parse(c.path, []byte(c.src))
		if len(f.Types) != 1 || f.Types[0].Stereotype != c.want {
			t.Errorf("%s: %+v", c.src, f.Types)
		}
	}
	f := Parse("A.java", []byte("class A extends a.b.Base {}\nclass B extends java.util.ArrayList<String> {}\n"))
	if f.Types[0].Extends[0] != "Base" || f.Types[1].Extends[0] != "ArrayList" {
		t.Errorf("base types %v %v", f.Types[0].Extends, f.Types[1].Extends)
	}
	if AnnotationName(`@org.springframework.Service("x")`) != "Service" || AnnotationName("@Getter") != "Getter" {
		t.Error("annotation names")
	}
}

func TestLineChecksOnlyOnChangedLines(t *testing.T) {
	src := `class A {
  OrderRepository orders;
  private static final SimpleDateFormat F = new SimpleDateFormat("yyyy");
  public boolean equals(Object o) { return true; }
  void a(java.util.List<Long> ids) {
    for (Long id : ids) { orders.findById(id); }
  }
}
`
	f := Parse("a/src/main/java/A.java", []byte(src))
	rules := func(lines map[int]bool) []string {
		var out []string
		for _, is := range Check(f, lines) {
			out = append(out, is.Rule)
		}
		slices.Sort(out)
		return out
	}
	if got := rules(map[int]bool{6: true}); !slices.Equal(got, []string{"slow-call-in-loop"}) {
		t.Errorf("only the loop changed: %v", got)
	}
	if got := rules(map[int]bool{1: true}); len(got) != 0 {
		t.Errorf("nothing flagged changed: %v", got)
	}
	if got := rules(nil); len(got) != 3 {
		t.Errorf("the whole file: %v", got)
	}
	var b strings.Builder
	b.WriteString("class B {\n  int a(int x) {\n")
	for i := 0; i < 16; i++ {
		b.WriteString("    if (x == " + itoa(i) + ") { return " + itoa(i) + "; }\n")
	}
	b.WriteString("    return -1;\n  }\n}\n")
	got := Check(Parse("a/src/main/java/B.java", []byte(b.String())), nil)
	if len(got) != 1 || got[0].Rule != "complex-method" || !strings.Contains(got[0].Title, "17 paths") {
		t.Errorf("complex method: %+v", got)
	}
}
