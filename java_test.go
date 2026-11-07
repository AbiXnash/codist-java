package java

import "testing"

const controller = `package com.shop.order;

import com.shop.common.Money;
import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/orders")
public class OrderController {
    private final OrderService service;

    OrderController(OrderService service) { this.service = service; }

    @GetMapping("/{id}")
    public Order get(@PathVariable long id) { return service.find(id); }

    @PostMapping
    public Order create(@RequestBody Order o) { return service.save(o); }

    @RequestMapping(value = "/old", method = RequestMethod.DELETE)
    public void old() {}
}`

const service = `package com.shop.order;

import org.springframework.stereotype.Service;
import java.util.List;

@Service
public class OrderService {
    static final String PREFIX = "ORD";
    private final OrderRepository orders;
    private final CatalogClient catalog;

    OrderService(OrderRepository orders, CatalogClient catalog) { this.orders = orders; this.catalog = catalog; }

    public Order find(long id) { return orders.findById(id).orElseThrow(); }

    public Order save(Order o) {
        for (Item i : o.items()) {
            catalog.price(i.sku());
        }
        return orders.save(o);
    }

    void total(List<Order> all) {
        int n = 0;
        for (int i = 0; i < 3; i++) { for (int j = 0; j < 3; j++) { n++; } }
        all.forEach(x -> orders.save(x));
    }
}`

const client = `package com.shop.order;

@FeignClient(name = "catalog-service")
public interface CatalogClient {
    @GetMapping("/products/{sku}/price")
    long price(@PathVariable String sku);
}`

func build() *Index {
	return Build(map[string][]byte{
		"order/src/main/java/com/shop/order/OrderController.java": []byte(controller),
		"order/src/main/java/com/shop/order/OrderService.java":    []byte(service),
		"order/src/main/java/com/shop/order/CatalogClient.java":   []byte(client),
		"order/src/main/java/com/shop/order/OrderApp.java":        []byte("package com.shop.order;\n@SpringBootApplication\npublic class OrderApp {}"),
		"catalog/src/main/java/com/shop/catalog/CatalogApp.java":  []byte("package com.shop.catalog;\n@SpringBootApplication\npublic class CatalogApp {}"),
	}, map[string][]byte{
		"order/pom.xml":   nil,
		"catalog/pom.xml": nil,
		"order/src/main/resources/application.yml":   []byte("server:\n  port: 8081\nspring:\n  application:\n    name: order-service\n"),
		"catalog/src/main/resources/application.yml": []byte("server:\n  port: 8082\nspring:\n  application:\n    name: catalog-service\n"),
	})
}

func TestEndpointsAndStereotypes(t *testing.T) {
	ix := build()
	c := ix.ByID["com.shop.order.OrderController"]
	if c == nil || c.Stereotype != "controller" || len(c.Endpoints) != 3 {
		t.Fatalf("controller: %+v", c)
	}
	want := map[string]string{"get": "GET /orders/{id}", "create": "POST /orders", "old": "DELETE /orders/old"}
	for _, e := range c.Endpoints {
		if got := e.Verb + " " + e.Path; got != want[e.Handler] {
			t.Errorf("%s: %s, want %s", e.Handler, got, want[e.Handler])
		}
	}
	if s := ix.ByID["com.shop.order.OrderService"]; s.Stereotype != "service" || s.Fields[0].Value != `"ORD"` {
		t.Errorf("service: %+v", s.Fields)
	}
	if ix.ByID["com.shop.order.CatalogClient"].Stereotype != "client" {
		t.Error("Feign client not recognised")
	}
}

func TestModulesAndFeign(t *testing.T) {
	ix := build()
	order, catalog := ix.Module("order"), ix.Module("catalog")
	if order == nil || catalog == nil {
		t.Fatalf("modules: %+v", ix.Modules)
	}
	if order.Port != "8081" || order.App != "order-service" || order.Kind != "service" || order.Endpoints != 3 {
		t.Errorf("order module: %+v", order)
	}
	if len(order.Calls) != 1 || order.Calls[0] != "catalog" || len(catalog.CalledBy) != 1 {
		t.Errorf("Feign link: calls %v, called by %v", order.Calls, catalog.CalledBy)
	}
}

func TestCallsResolve(t *testing.T) {
	ix := build()
	has := func(from, to string) bool {
		for _, l := range ix.Links {
			if l.From == from && l.To == to {
				return true
			}
		}
		return false
	}
	if !has("com.shop.order.OrderController#get", "com.shop.order.OrderService#find") {
		t.Error("controller to service call not linked")
	}
	if !has("com.shop.order.OrderService#save", "com.shop.order.CatalogClient#price") {
		t.Error("service to Feign client call not linked")
	}
	if callers := ix.Callers("com.shop.order.OrderService#find"); len(callers) != 1 {
		t.Errorf("callers: %v", callers)
	}
}

func TestCost(t *testing.T) {
	s := build().ByID["com.shop.order.OrderService"]
	save, total := s.Method("save"), s.Method("total")
	if save.Cost.LoopIO != 1 || save.Cost.Remote != 1 || save.Cost.Estimate != "O(n) round trips" {
		t.Errorf("save: %+v", save.Cost)
	}
	if total.Cost.LoopDepth != 1 || total.Cost.LoopIO != 1 {
		t.Errorf("total: small fixed loops do not count, forEach does: %+v", total.Cost)
	}
	if typ, m := build().TypeAt("order/src/main/java/com/shop/order/OrderService.java", 19); typ == nil || m == nil || m.Name != "save" {
		t.Errorf("TypeAt: %v %v", typ, m)
	}
}
