package mgmt

import "net/http"

// RegisteredRoutes builds the API of s and returns every registered route pattern.
func RegisteredRoutes(s *Server) []string {
	s.init()
	var out []string
	s.build(func(next http.Handler, pattern string) http.Handler { out = append(out, pattern); return next }, true)
	return out
}

// RouteBindings exposes how each documented route finds its repository.
func RouteBindings() map[string]string {
	out := map[string]string{}
	for p, m := range apiRoutes {
		out[p] = m.Bind.String()
	}
	return out
}

// RouteDocs exposes summary and tag of each documented route.
func RouteDocs() map[string][2]string {
	out := map[string][2]string{}
	for p, m := range apiRoutes {
		out[p] = [2]string{m.Summary, m.Tag}
	}
	return out
}

// RoutesMarkdown exposes the generated route table of docs/api.md.
func RoutesMarkdown() string { return routesMarkdown() }

// RouteBodies exposes the documented request body value of each route.
func RouteBodies() map[string]any {
	out := map[string]any{}
	for p, m := range apiRoutes {
		if m.Body != nil {
			out[p] = m.Body
		}
	}
	return out
}
