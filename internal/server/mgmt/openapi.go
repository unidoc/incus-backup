package mgmt

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/server/auth"
)

// OpenAPI returns the OpenAPI 3.1 document of /api/v1, generated from the route metadata
// (routes_meta.go) and the role table (perms.go); schemas are derived from the Go types by reflection.
// It needs no database: `incus-backup-server openapi` prints it offline.
func OpenAPI(version string) []byte {
	g := &schemaGen{comps: map[string]any{}, names: map[reflect.Type]string{}, taken: map[string]reflect.Type{}}
	errRef := g.schema(reflect.TypeOf(api.Error{}))
	paths := map[string]map[string]any{}
	pats := make([]string, 0, len(apiRoutes))
	for p := range apiRoutes {
		if _, ok := perms[p]; ok {
			pats = append(pats, p)
		}
	}
	sort.Strings(pats)
	for _, pat := range pats {
		m := apiRoutes[pat]
		method, path, _ := strings.Cut(pat, " ")
		op := map[string]any{
			"operationId":          operationID(method, path),
			"summary":              m.Summary,
			"tags":                 []string{m.Tag},
			"description":          describeAccess(pat, m),
			"x-role":               roleName(perms[pat]),
			"x-repository-binding": m.Bind.String(),
		}
		if publicRoutes[pat] {
			op["security"] = []any{}
		}
		var params []any
		for _, name := range pathParams(path) {
			params = append(params, map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string"}, "description": pathParamDesc(pat, name)})
		}
		for _, q := range m.Query {
			typ := q.Type
			if typ == "" {
				typ = "string"
			}
			params = append(params, map[string]any{"name": q.Name, "in": "query", "required": q.Required, "schema": map[string]any{"type": typ}, "description": q.Desc})
		}
		if params != nil {
			op["parameters"] = params
		}
		if m.Body != nil {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": g.schema(reflect.TypeOf(m.Body))}}}
		}
		resp := map[string]any{}
		code := m.Status
		switch {
		case m.Kind == kindBinary:
			if code == 0 {
				code = 200
			}
			resp[fmt.Sprint(code)] = map[string]any{"description": "ciphertext, as stored", "content": map[string]any{"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "contentMediaType": "application/octet-stream"}}}}
		case m.Kind == kindSSE:
			resp["200"] = map[string]any{"description": "event stream: `event: <type>` and `data: {\"type\":...,\"data\":...}` lines; a comment line every 20 s", "content": map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}}}
		case m.Resp != nil:
			if code == 0 {
				code = 200
			}
			resp[fmt.Sprint(code)] = map[string]any{"description": "success", "content": map[string]any{"application/json": map[string]any{"schema": g.schema(reflect.TypeOf(m.Resp))}}}
		default:
			if code == 0 {
				code = 204
			}
			resp[fmt.Sprint(code)] = map[string]any{"description": "done"}
		}
		errResp := func(d string) map[string]any {
			return map[string]any{"description": d, "content": map[string]any{"application/json": map[string]any{"schema": errRef}}}
		}
		resp["400"] = errResp("bad request (the message says what to fix)")
		if !publicRoutes[pat] {
			resp["401"] = errResp("not signed in")
			resp["403"] = errResp("the role (on this repository) does not allow it, or a CSRF token is missing")
		}
		resp["404"] = errResp("not found (also: an id of a repository the caller may not see)")
		resp["409"] = errResp("conflict (exists, protected, owned by another server)")
		resp["500"] = errResp("server error")
		if m.Bind == bindQuery || m.Bind == bindPath || m.Bind == bindBody {
			resp["503"] = errResp("repository is read-only or offline (maintenance mode)")
		}
		op["responses"] = resp
		oapiPath := path
		if paths[oapiPath] == nil {
			paths[oapiPath] = map[string]any{}
		}
		paths[oapiPath][strings.ToLower(method)] = op
	}
	tagSet := map[string]bool{}
	for _, m := range apiRoutes {
		tagSet[m.Tag] = true
	}
	var tags []any
	tagNames := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tagNames = append(tagNames, t)
	}
	sort.Strings(tagNames)
	for _, t := range tagNames {
		d := tagDescriptions[t]
		if d == "" {
			d = t
		}
		tags = append(tags, map[string]any{"name": t, "description": d})
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "incus-backup management API",
			"license": map[string]any{"name": "Apache-2.0", "identifier": "Apache-2.0"},
			"version": version,
			"description": "The management API of incus-backup-server (docs/api.md). Browsers use the session cookie plus the " +
				"X-CSRF-Token header on state-changing requests; automation uses `Authorization: Bearer ibt_...` API tokens. " +
				"Roles: readonly < restore < operator < admin; per-repository roles narrow them (docs/permissions.md). " +
				"`x-role` is the role a route needs, `x-repository-binding` how its repository is found.",
		},
		"servers": []any{map[string]any{"url": "/"}},
		"tags":    tags,
		"paths":   paths,
		"components": map[string]any{
			"schemas": g.comps,
			"securitySchemes": map[string]any{
				"session": map[string]any{"type": "apiKey", "in": "cookie", "name": cookieName, "description": "web session (sign in with POST /api/v1/session); send X-CSRF-Token on POST/PUT/DELETE"},
				"token":   map[string]any{"type": "http", "scheme": "bearer", "description": "API token (ibt_...) from POST /api/v1/me/tokens"},
			},
		},
		"security": []any{map[string]any{"session": []any{}}, map[string]any{"token": []any{}}},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // only static data: a bug
	}
	return append(b, '\n')
}

var tagDescriptions = map[string]string{
	"Alerts":        "Alerts raised by the notification engine, acknowledgement and silences",
	"Auth":          "Sign-in, session and first-run setup",
	"Capacity":      "Stored bytes, growth and forecasts",
	"Certificates":  "The server CA, the server certificate and sync peers",
	"Credentials":   "Sealed S3 credentials (secrets are never returned)",
	"Dashboard":     "Aggregated read models for the dashboard",
	"Hosts":         "Backup hosts, enrolment and backup runs",
	"Jobs":          "Host jobs (restores, file listings) and restore tests",
	"Maintenance":   "Prune, garbage collection, verification, reconcile",
	"Me":            "The signed-in user's own password, two-factor, sessions and tokens",
	"Notifications": "Channels, routing rules, deliveries and alert conditions",
	"Objects":       "Storage objects (backup groups)",
	"Policies":      "Backup and retention policies and their assignments",
	"Repositories":  "Repositories (datastores) and their maintenance mode",
	"Settings":      "Server settings and the schedule preview",
	"Snapshots":     "Snapshots, protection, notes and the encrypted material the browser decrypts",
	"Sync":          "Sync jobs to other servers",
	"System":        "Liveness, version, events and this document",
	"Traffic":       "Upload and download rate limits of the agent API",
	"Users":         "Users, per-repository access, API tokens and the audit log",
}

var paramRE = regexp.MustCompile(`\{([a-z_]+)\}`)

func pathParams(path string) []string {
	var out []string
	for _, m := range paramRE.FindAllStringSubmatch(path, -1) {
		out = append(out, m[1])
	}
	return out
}

func pathParamDesc(pat, name string) string {
	switch name {
	case "name":
		switch {
		case strings.Contains(pat, "/repositories/"):
			return "repository name"
		case strings.Contains(pat, "/users/"):
			return "user name"
		case strings.Contains(pat, "/hosts/"):
			return "host name"
		case strings.Contains(pat, "/policies/"):
			return "policy name"
		case strings.Contains(pat, "/sync/"):
			return "sync job name"
		case strings.Contains(pat, "/credentials/"):
			return "credential name"
		case strings.Contains(pat, "/traffic/"):
			return "traffic rule name"
		}
	case "group":
		return "object group id (32 hex digits)"
	case "snap":
		return "snapshot id"
	case "id":
		return "identifier"
	}
	return name
}

func operationID(method, path string) string {
	p := strings.TrimPrefix(path, "/api/v1/")
	p = strings.NewReplacer("{", "by_", "}", "", "/", "_", "-", "_", ".", "_").Replace(p)
	return strings.ToLower(method) + "_" + p
}

func roleName(p auth.Perm) string {
	switch p {
	case auth.PermRead:
		return "readonly"
	case auth.PermRestore:
		return "restore"
	case auth.PermOperate:
		return "operator"
	}
	return "admin"
}

func describeAccess(pat string, m apiRoute) string {
	role := roleName(perms[pat])
	if publicRoutes[pat] {
		return "No login needed."
	}
	switch m.Bind {
	case bindSelf:
		return "Any signed-in principal (" + role + ")."
	case bindQuery:
		return "Needs **" + role + "** on the repository named by `?repository=`."
	case bindPath:
		return "Needs **" + role + "** on the repository `{name}`."
	case bindBody:
		return "Needs **" + role + "** on the repository named in the body (`repository`), else `?repository=`."
	case bindJob:
		return "Needs **" + role + "** on the job's repository; other repositories' jobs answer 404."
	case bindAlert:
		return "Needs **" + role + "** on the alert's repository (global role for alerts without one); others answer 404."
	case bindList:
		return "Needs **" + role + "** somewhere; the answer contains only repositories the caller may read."
	}
	return "Needs the global role **" + role + "**."
}

// ---- reflection-based JSON Schema (2020-12, as OpenAPI 3.1 uses) -----------------------------------

type schemaGen struct {
	comps map[string]any
	names map[reflect.Type]string
	taken map[string]reflect.Type
}

var (
	timeType     = reflect.TypeOf(time.Time{})
	durationType = reflect.TypeOf(time.Duration(0))
	rawType      = reflect.TypeOf(json.RawMessage{})
	jsonMarshal  = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshal  = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	nameCleanRE  = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

func (g *schemaGen) componentName(t reflect.Type) string {
	if n, ok := g.names[t]; ok {
		return n
	}
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	n := nameCleanRE.ReplaceAllString(pkg+"."+t.Name(), "_")
	for i := 2; g.taken[n] != nil && g.taken[n] != t; i++ {
		n = fmt.Sprintf("%s_%d", nameCleanRE.ReplaceAllString(pkg+"."+t.Name(), "_"), i)
	}
	g.names[t], g.taken[n] = n, t
	return n
}

func nullable(s map[string]any) map[string]any {
	if ref, ok := s["$ref"]; ok {
		return map[string]any{"anyOf": []any{map[string]any{"$ref": ref}, map[string]any{"type": "null"}}}
	}
	if t, ok := s["type"].(string); ok {
		c := map[string]any{}
		for k, v := range s {
			c[k] = v
		}
		c["type"] = []any{t, "null"}
		return c
	}
	return s
}

func (g *schemaGen) schema(t reflect.Type) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == durationType:
		return map[string]any{"type": "integer", "description": "duration in nanoseconds"}
	case t == rawType:
		return map[string]any{"description": "any JSON value"}
	}
	if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface {
		if t.Implements(jsonMarshal) || reflect.PointerTo(t).Implements(jsonMarshal) {
			return map[string]any{"description": "custom JSON encoding (" + t.String() + ")"}
		}
		if t.Implements(textMarshal) || reflect.PointerTo(t).Implements(textMarshal) {
			return map[string]any{"type": "string", "description": t.String()}
		}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return nullable(g.schema(t.Elem()))
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Interface:
		return map[string]any{"description": "any JSON value"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schema(t.Elem())}
	case reflect.Struct:
		if t.Name() == "" {
			return g.object(t)
		}
		name := g.componentName(t)
		if _, done := g.comps[name]; !done {
			g.comps[name] = map[string]any{} // placeholder: recursive types
			g.comps[name] = g.object(t)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	return map[string]any{"description": "unsupported type " + t.String()}
}

func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	g.fields(t, props, &required)
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		o["required"] = required
	}
	return o
}

func (g *schemaGen) fields(t reflect.Type, props map[string]any, required *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			et := ft
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				g.fields(et, props, required)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		var s map[string]any
		if strings.Contains(","+opts+",", ",string,") {
			s = map[string]any{"type": "string"}
		} else {
			s = g.schema(ft)
		}
		if d := f.Tag.Get("doc"); d != "" {
			if _, isRef := s["$ref"]; isRef {
				s = map[string]any{"allOf": []any{s}, "description": d}
			} else {
				c := map[string]any{"description": d}
				for k, v := range s {
					if k != "description" {
						c[k] = v
					}
				}
				s = c
			}
		}
		props[name] = s
		optional := strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,") || ft.Kind() == reflect.Pointer
		if !optional {
			*required = append(*required, name)
		}
	}
}

// routesMarkdown is the route table of docs/api.md ("All routes"), generated from the same metadata
// (TestAPIDocListsEveryRoute keeps the committed copy equal; IB_WRITE_API_DOC=1 rewrites it).
func routesMarkdown() string {
	type row struct{ tag, path, method, line string }
	var rows []row
	for pat, m := range apiRoutes {
		perm, ok := perms[pat]
		if !ok {
			continue
		}
		method, path, _ := strings.Cut(pat, " ")
		role := roleName(perm)
		if publicRoutes[pat] {
			role = "public"
		}
		bind := m.Bind.String()
		rows = append(rows, row{m.Tag, path, method, fmt.Sprintf("| %s | `%s %s` | %s | %s | %s |", m.Tag, method, strings.TrimPrefix(path, "/api/v1"), role, bind, strings.ReplaceAll(m.Summary, "|", "\\|"))})
	}
	order := map[string]int{"GET": 0, "POST": 1, "PUT": 2, "DELETE": 3}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.tag != b.tag {
			return a.tag < b.tag
		}
		if a.path != b.path {
			return a.path < b.path
		}
		return order[a.method] < order[b.method]
	})
	var b strings.Builder
	b.WriteString("| area | route | role | repository | what it does |\n|---|---|---|---|---|\n")
	for _, r := range rows {
		b.WriteString(r.line + "\n")
	}
	return b.String()
}
