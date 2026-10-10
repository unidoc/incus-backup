package config

import (
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// RedactedValue replaces every secret in Redacted.
const RedactedValue = "[redacted]"

// secretKey reports whether a configuration key names something secret.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"password", "passwd", "secret", "token", "credential", "private", "key"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// RedactURL removes the password (and any query string, which may carry one) from a connection URL.
// A value that is not a URL (a libpq key=value string) is replaced entirely.
func RedactURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return RedactedValue
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	return u.String()
}

// Redacted is the configuration for a support report: database_url without its password, and every
// value whose key looks secret replaced (key files are paths, but are redacted too: a path is not
// needed to diagnose anything and the rule stays simple).
func (c *Config) Redacted() map[string]any {
	b, err := yaml.Marshal(c)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return map[string]any{"error": err.Error()}
	}
	if v, ok := m["database_url"].(string); ok {
		m["database_url"] = RedactURL(v)
	}
	redactMap(m)
	return m
}

func redactMap(m map[string]any) {
	for k, v := range m {
		if k == "database_url" {
			continue
		}
		switch x := v.(type) {
		case map[string]any:
			redactMap(x)
		case []any:
			for _, e := range x {
				if mm, ok := e.(map[string]any); ok {
					redactMap(mm)
				}
			}
		default:
			if secretKey(k) && v != nil && v != "" {
				m[k] = RedactedValue
			}
		}
	}
}
