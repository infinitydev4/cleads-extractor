// Package obs makes logs readable through the API: a slog handler that tees
// warnings and errors to the database (asynchronously, never blocking the
// caller), an in-memory ring of recent records, and secret redaction applied
// before anything is stored or exposed.
package obs

import (
	"regexp"
	"strings"
)

var (
	// scheme://user:pass@host -> scheme://***@host (proxies, DATABASE_URL).
	reURLUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s@"']+@`)
	// "Bearer abc..." in header dumps and error messages.
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=\-]+`)
	// key=value / key: value pairs whose key names a secret.
	reKeyValue = regexp.MustCompile(`(?i)(\b(?:authorization|x-api-key|api[_-]?key|apikey|access[_-]?token|token|secret|password|passwd|pwd|signature|sig)["']?\s*[:=]\s*["']?)([^\s"'&,;}]+)`)
	// query parameters carrying credentials (Google uses ?key=).
	reQueryKey = regexp.MustCompile(`([?&](?:key|api_key|apikey|token|access_token|password|sig|signature)=)[^&\s"']+`)
)

const masked = "***"

// Redactor masks secrets in strings. It knows the generic shapes of
// credentials (URL userinfo, bearer tokens, key=value pairs) and, optionally,
// the exact secret values of this deployment. Safe for concurrent use.
type Redactor struct {
	exact *strings.Replacer
}

// NewRedactor returns a redactor that also masks the given literal secrets
// (empty and very short values are ignored to avoid masking common words).
func NewRedactor(secrets ...string) *Redactor {
	var pairs []string
	for _, s := range secrets {
		if len(s) >= 8 {
			pairs = append(pairs, s, masked)
		}
	}
	r := &Redactor{}
	if len(pairs) > 0 {
		r.exact = strings.NewReplacer(pairs...)
	}
	return r
}

// String returns s with secrets masked.
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	if r != nil && r.exact != nil {
		s = r.exact.Replace(s)
	}
	// Cheap prefilter: every pattern needs one of these characters.
	if !strings.ContainsAny(s, "@=:") && !strings.Contains(s, "earer") && !strings.Contains(s, "EARER") {
		return s
	}
	if strings.Contains(s, "@") {
		s = reURLUserinfo.ReplaceAllString(s, "${1}"+masked+"@")
	}
	s = reBearer.ReplaceAllString(s, "${1}"+masked)
	s = reQueryKey.ReplaceAllString(s, "${1}"+masked)
	return reKeyValue.ReplaceAllString(s, "${1}"+masked)
}

// Value redacts strings inside JSON-like values (maps, slices) in place and
// returns the result.
func (r *Redactor) Value(v any) any {
	switch x := v.(type) {
	case string:
		return r.String(x)
	case map[string]any:
		for k, e := range x {
			if secretKey(k) {
				x[k] = masked
				continue
			}
			x[k] = r.Value(e)
		}
		return x
	case []any:
		for i := range x {
			x[i] = r.Value(x[i])
		}
		return x
	}
	return v
}

// secretKey reports whether an attribute name designates a secret value.
func secretKey(k string) bool {
	switch strings.ToLower(k) {
	case "authorization", "x-api-key", "api_key", "apikey", "password", "secret", "token", "database_url", "dsn":
		return true
	}
	return false
}
