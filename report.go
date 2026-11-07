package java

import "sort"

// CostlyMethod is a method that may be slow: calls in a loop, nested loops, or many paths.
type CostlyMethod struct {
	Class  string `json:"class"`
	Node   string `json:"node"`
	Method string `json:"method"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Cost   Cost   `json:"cost"`
}

// Costly lists up to limit methods that may be slow, the likeliest first, and how many there are in all.
func Costly(ix *Index, limit int) ([]CostlyMethod, int) {
	out := []CostlyMethod{}
	for _, t := range ix.Types {
		for _, m := range t.Methods {
			if m.Cost.LoopIO > 0 || m.Cost.LoopDepth >= 2 || m.Cost.Complexity >= 8 {
				out = append(out, CostlyMethod{Class: t.Name, Node: t.ID, Method: m.Name, File: t.File, Line: m.Line, Cost: m.Cost})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i].Cost, out[j].Cost
		if x.LoopIO != y.LoopIO {
			return x.LoopIO > y.LoopIO
		}
		if x.LoopDepth != y.LoopDepth {
			return x.LoopDepth > y.LoopDepth
		}
		return x.Complexity > y.Complexity
	})
	return out[:min(limit, len(out))], len(out)
}

// LargeType is a class with its size. The field names are what the interface reads.
type LargeType struct {
	Name, Node, File string
	Lines, Members   int
}

// Largest lists up to limit classes by lines, largest first.
func Largest(ix *Index, limit int) []LargeType {
	types := append([]*Type(nil), ix.Types...)
	sort.SliceStable(types, func(i, j int) bool { return types[i].Lines() > types[j].Lines() })
	out := []LargeType{}
	for _, t := range types[:min(limit, len(types))] {
		out = append(out, LargeType{Name: t.Name, Node: t.ID, File: t.File, Lines: t.Lines(), Members: len(t.Fields) + len(t.Methods)})
	}
	return out
}

// FeignCall is a Feign client and the service it calls.
type FeignCall struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Client string   `json:"client"`
	Node   string   `json:"node"`
	Routes []string `json:"routes"`
}

// Route is an HTTP route a controller serves, with how many Feign clients in the repository call it.
type Route struct {
	Service string `json:"service"`
	Route   string `json:"route"`
	Handler string `json:"handler"`
	Class   string `json:"class"`
	Node    string `json:"node"`
	Callers int    `json:"callers"`
}

// Services lists the Feign calls between modules, and every route with its callers.
func Services(ix *Index) ([]FeignCall, []Route) {
	callers := map[string]int{}
	calls := []FeignCall{}
	for _, t := range ix.Types {
		if t.Stereotype != "client" {
			continue
		}
		target := ""
		for _, m := range ix.Modules {
			if m.App == t.Feign || m.Name == t.Feign {
				target = m.Name
			}
		}
		routes := []string{}
		for _, e := range t.Endpoints {
			routes = append(routes, e.Verb+" "+e.Path)
			callers[target+" "+e.Verb+" "+e.Path]++
		}
		to := target
		if to == "" {
			to = t.Feign
		}
		calls = append(calls, FeignCall{From: t.Module, To: to, Client: t.Name, Node: t.ID, Routes: routes})
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].From+calls[i].Client < calls[j].From+calls[j].Client })
	routes := []Route{}
	for _, t := range ix.Types {
		if t.Stereotype == "client" {
			continue
		}
		for _, e := range t.Endpoints {
			routes = append(routes, Route{Service: t.Module, Route: e.Verb + " " + e.Path, Handler: e.Handler + "()", Class: t.Name, Node: t.ID,
				Callers: callers[t.Module+" "+e.Verb+" "+e.Path]})
		}
	}
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Service != routes[j].Service {
			return routes[i].Service < routes[j].Service
		}
		return routes[i].Route < routes[j].Route
	})
	return calls, routes
}

// Hotspot is a file that changes often and has open issues.
type Hotspot struct {
	Path    string `json:"path"`
	Commits int    `json:"commits"`
	Open    int    `json:"open"`
	Lines   int    `json:"lines"`
}

// Hotspots ranks the project's Java files by how often they change times their open issues plus one, and lists
// up to limit of them. churn counts commits per file; open counts open issues per file.
func Hotspots(ix *Index, churn, open map[string]int, limit int) []Hotspot {
	lines := map[string]int{}
	for _, t := range ix.Types {
		lines[t.File] = max(lines[t.File], t.End)
	}
	out := []Hotspot{}
	for p, n := range churn {
		if l, ok := lines[p]; ok {
			out = append(out, Hotspot{Path: p, Commits: n, Open: open[p], Lines: l})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i].Commits*(out[i].Open+1), out[j].Commits*(out[j].Open+1)
		if x != y {
			return x > y
		}
		return out[i].Path < out[j].Path
	})
	return out[:min(limit, len(out))]
}

// ModuleOfFile maps each Java file to its module's name.
func ModuleOfFile(ix *Index) map[string]string {
	out := map[string]string{}
	for _, t := range ix.Types {
		out[t.File] = t.Module
	}
	return out
}
