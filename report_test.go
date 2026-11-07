package java

import (
	"strings"
	"testing"
)

func TestGraph(t *testing.T) {
	ix := shop(t)
	kinds := map[string]GraphEdge{}
	for _, e := range GraphEdges(ix) {
		kinds[e.Source+"->"+e.Target] = e
	}
	feign := kinds["com.shop.order.PaymentClient->com.shop.payment.PaymentController"]
	if feign.Kind != "feign" || strings.Join(feign.Via, ",") != "POST /payments/{}" {
		t.Errorf("feign edge %+v", feign)
	}
	if kinds["com.shop.order.OrderController->com.shop.order.OrderService"].Kind != "injects" ||
		kinds["com.shop.order.OrderService->com.shop.order.Order"].Kind != "uses" {
		t.Error("injects and uses")
	}
	n := GraphNodeOf(ix.ByID["com.shop.order.OrderService"])
	if n.Stereotype != "service" || n.Members[0].Kind != "field" || n.Members[2].Kind != "constructor" || n.Members[3].Cost == nil {
		t.Errorf("node %+v", n)
	}
	if c := GraphNodeOf(ix.ByID["com.shop.payment.PaymentController"]); strings.Join(c.Endpoints, "") != "POST /payments/{id}  → charge()" {
		t.Errorf("endpoints %v", c.Endpoints)
	}
	if strings.Join(dedupe([]string{"a", "b", "a"}), "") != "ab" {
		t.Error("dedupe")
	}
}

func TestReport(t *testing.T) {
	ix := shop(t)
	costly, total := Costly(ix, 15)
	if total == 0 || costly[0].Method != "payAll" || costly[0].Cost.LoopIO == 0 {
		t.Errorf("costly %+v", costly)
	}
	if one, _ := Costly(ix, 0); len(one) != 0 {
		t.Error("limit")
	}
	big := Largest(ix, 2)
	if len(big) != 2 || big[0].Lines < big[1].Lines || big[0].Members == 0 {
		t.Errorf("largest %+v", big)
	}
	calls, routes := Services(ix)
	if len(calls) != 1 || calls[0].From != "order-service" || calls[0].To != "payment-service" || calls[0].Routes[0] != "POST /payments/{orderId}" {
		t.Errorf("calls %+v", calls)
	}
	if len(routes) != 2 || routes[0].Service != "order-service" || routes[1].Handler != "charge()" {
		t.Errorf("routes %+v", routes)
	}
	svc := "order-service/src/main/java/com/shop/order/OrderService.java"
	hot := Hotspots(ix, map[string]int{svc: 3, "README.md": 9, "order-service/src/main/java/com/shop/order/Order.java": 3},
		map[string]int{svc: 1}, 12)
	if len(hot) != 2 || hot[0].Path != svc || hot[0].Open != 1 || hot[0].Lines == 0 {
		t.Errorf("hotspots %+v", hot)
	}
	if ModuleOfFile(ix)[svc] != "order-service" {
		t.Error("module of file")
	}
	// A client whose service is not in the repository still names it.
	lone := Build(map[string][]byte{"a/src/main/java/C.java": []byte(`@FeignClient(name = "elsewhere") interface C { @GetMapping("/x") String x(); }`)}, nil)
	if c, _ := Services(lone); len(c) != 1 || c[0].To != "elsewhere" {
		t.Errorf("lone client %+v", c)
	}
}
