// Package normalize turns heterogeneous source values into canonical forms
// used for storage, deduplication and CRM export. Every function is pure and
// allocation-conscious because it runs on the hot path of every record.
package normalize

import (
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

type dialPlan struct {
	cc     string // country calling code
	trunk  string // national trunk prefix to strip
	nsnMin int    // national significant number length bounds
	nsnMax int
}

// A compact table covering the markets CLEADS targets. Unknown countries fall
// back to a permissive international check.
var dialPlans = map[string]dialPlan{
	"FR": {"33", "0", 9, 9}, "BE": {"32", "0", 8, 9}, "CH": {"41", "0", 9, 9},
	"LU": {"352", "", 4, 11}, "MC": {"377", "", 8, 9}, "DE": {"49", "0", 6, 13},
	"ES": {"34", "", 9, 9}, "IT": {"39", "", 6, 11}, "PT": {"351", "", 9, 9},
	"NL": {"31", "0", 9, 9}, "GB": {"44", "0", 9, 10}, "IE": {"353", "0", 7, 9},
	"US": {"1", "1", 10, 10}, "CA": {"1", "1", 10, 10}, "MA": {"212", "0", 9, 9},
	"TN": {"216", "", 8, 8}, "DZ": {"213", "0", 8, 9}, "SN": {"221", "", 9, 9},
	"CI": {"225", "", 10, 10}, "AT": {"43", "0", 4, 13}, "PL": {"48", "", 9, 9},
	"RE": {"262", "0", 9, 9}, "GP": {"590", "0", 9, 9}, "MQ": {"596", "0", 9, 9},
}

// Phone returns the E.164 form of raw ("+33123456789") when it can be
// determined, the digits otherwise, or "" for garbage.
func Phone(raw, country string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	plus := false
scan:
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && b.Len() == 0:
			plus = true
		case r == ';' || r == ',' || r == '/' || unicode.IsLetter(r):
			// extension ("x12", "poste 12") or list separator: keep the first number
			if b.Len() > 0 {
				break scan
			}
		}
	}
	d := b.String()
	if len(d) < 4 {
		return ""
	}
	if !plus && strings.HasPrefix(d, "00") {
		plus, d = true, d[2:]
	}
	if plus {
		if len(d) < 8 || len(d) > 15 {
			return ""
		}
		// "+33 (0)1 23..." is a common French mistake.
		if p, ok := dialPlans[strings.ToUpper(country)]; ok && p.trunk != "" &&
			strings.HasPrefix(d, p.cc+p.trunk) && len(d)-len(p.cc)-len(p.trunk) >= p.nsnMin &&
			len(d)-len(p.cc) > p.nsnMax {
			d = p.cc + d[len(p.cc)+len(p.trunk):]
		}
		return "+" + d
	}
	p, ok := dialPlans[strings.ToUpper(country)]
	if !ok {
		return d
	}
	nsn := d
	if strings.HasPrefix(nsn, p.cc) && len(nsn)-len(p.cc) >= p.nsnMin && len(nsn)-len(p.cc) <= p.nsnMax {
		nsn = nsn[len(p.cc):]
	} else if p.trunk != "" && strings.HasPrefix(nsn, p.trunk) && len(nsn)-len(p.trunk) >= p.nsnMin {
		nsn = nsn[len(p.trunk):]
	}
	if len(nsn) < p.nsnMin || len(nsn) > p.nsnMax {
		return d
	}
	return "+" + p.cc + nsn
}

// Domains that are not a business website but a profile hosted elsewhere.
var socialHosts = map[string]string{
	"facebook.com": "facebook", "fb.com": "facebook", "m.facebook.com": "facebook",
	"instagram.com": "instagram", "linkedin.com": "linkedin", "fr.linkedin.com": "linkedin",
	"twitter.com": "x", "x.com": "x", "youtube.com": "youtube", "tiktok.com": "tiktok",
}

// Website canonicalizes a URL. It returns the canonical URL and its
// registrable-ish host (without "www."). Social profile URLs are reported
// through social so callers can file them correctly.
func Website(raw string) (canonical, host, social string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", ""
	}
	h := strings.ToLower(u.Hostname())
	h = strings.TrimSuffix(h, ".")
	if !strings.Contains(h, ".") {
		return "", "", ""
	}
	port := u.Port()
	u.Host = h
	if port != "" && port != "80" && port != "443" {
		u.Host = h + ":" + port
	}
	u.Fragment = ""
	u.User = nil
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "utm_") || lk == "gclid" || lk == "fbclid" || lk == "y_source" || lk == "hl" {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
	}
	if u.Path == "/" {
		u.Path = ""
	}
	host = strings.TrimPrefix(h, "www.")
	if s, ok := socialHosts[host]; ok {
		social = s
	}
	return u.String(), host, social
}

var accentStripper = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// Fold lowercases and strips diacritics: "Café Étoile" -> "cafe etoile".
func Fold(s string) string {
	if s == "" {
		return ""
	}
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if !ascii {
		if out, _, err := transform.String(accentStripper, s); err == nil {
			s = out
		}
	}
	return strings.ToLower(s)
}

var legalForms = map[string]struct{}{
	"sarl": {}, "sas": {}, "sasu": {}, "eurl": {}, "sa": {}, "sci": {}, "snc": {}, "scop": {},
	"selarl": {}, "ei": {}, "eirl": {}, "ltd": {}, "llc": {}, "inc": {}, "gmbh": {}, "bv": {},
	"srl": {}, "sprl": {}, "spa": {}, "plc": {}, "co": {}, "cie": {}, "et": {}, "the": {},
	"le": {}, "la": {}, "les": {}, "l": {}, "de": {}, "du": {}, "des": {},
}

// NameKey is the comparison key for business names: folded, punctuation
// removed, legal forms and stop words dropped.
func NameKey(name string) string {
	f := Fold(name)
	var b strings.Builder
	b.Grow(len(f))
	word := make([]rune, 0, 16)
	flush := func() {
		if len(word) == 0 {
			return
		}
		w := string(word)
		word = word[:0]
		if _, skip := legalForms[w]; skip {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	for _, r := range f {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
		} else {
			flush()
		}
	}
	flush()
	if b.Len() == 0 {
		return strings.TrimSpace(f)
	}
	return b.String()
}

var junkEmailDomains = map[string]struct{}{
	"example.com": {}, "example.org": {}, "domain.com": {}, "email.com": {}, "sentry.io": {},
	"wixpress.com": {}, "sentry.wixpress.com": {}, "yourdomain.com": {}, "votredomaine.fr": {},
	"exemple.fr": {}, "exemple.com": {}, "godaddy.com": {}, "mysite.com": {}, "company.com": {},
	"yourcompany.com": {}, "domaine.com": {}, "domaine.fr": {}, "monsite.fr": {}, "site.com": {},
}

// Placeholder mailboxes found in website templates.
var junkEmailLocals = map[string]struct{}{
	"you": {}, "your": {}, "yourname": {}, "name": {}, "nom": {}, "email": {}, "mail": {}, "user": {},
	"username": {}, "example": {}, "test": {}, "john.doe": {}, "jane.doe": {}, "prenom.nom": {},
	"votre.email": {}, "votremail": {}, "adresse": {},
}

var junkEmailSuffixes = []string{".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".css", ".js"}

// Email validates and lowercases an address, returning "" for invalid or
// obviously technical/placeholder ones.
func Email(raw string) string {
	e := strings.ToLower(strings.TrimSpace(raw))
	e = strings.TrimPrefix(e, "mailto:")
	if i := strings.IndexByte(e, '?'); i >= 0 {
		e = e[:i]
	}
	e = strings.Trim(e, ".,;:<>()[]\"'")
	at := strings.LastIndexByte(e, '@')
	if at <= 0 || at == len(e)-1 || len(e) > 254 {
		return ""
	}
	local, dom := e[:at], e[at+1:]
	if len(local) > 64 || !strings.Contains(dom, ".") || strings.Contains(dom, "..") {
		return ""
	}
	for _, s := range junkEmailSuffixes {
		if strings.HasSuffix(e, s) {
			return ""
		}
	}
	if _, junk := junkEmailDomains[dom]; junk {
		return ""
	}
	if _, junk := junkEmailLocals[local]; junk {
		return ""
	}
	tld := dom[strings.LastIndexByte(dom, '.')+1:]
	if len(tld) < 2 {
		return ""
	}
	for _, r := range e {
		if r > unicode.MaxASCII || unicode.IsSpace(r) {
			return ""
		}
	}
	return e
}

// PostalCodeFR extracts a 5-digit French postal code from a free-form address.
func PostalCodeFR(addr string) string {
	n := 0
	for i := 0; i < len(addr); i++ {
		if addr[i] >= '0' && addr[i] <= '9' {
			n++
			if n == 5 && (i+1 == len(addr) || addr[i+1] < '0' || addr[i+1] > '9') &&
				(i-5 < 0 || addr[i-5] < '0' || addr[i-5] > '9') {
				return addr[i-4 : i+1]
			}
		} else {
			n = 0
		}
	}
	return ""
}

// ValidSIRET checks length and the Luhn checksum (La Poste SIRETs excepted).
func ValidSIRET(s string) bool {
	if len(s) != 14 {
		return false
	}
	if strings.HasPrefix(s, "356000000") {
		sum := 0
		for i := 0; i < 14; i++ {
			sum += int(s[i] - '0')
		}
		return sum%5 == 0 || luhn(s)
	}
	return luhn(s)
}

// ValidSIREN checks a 9-digit SIREN with the Luhn checksum.
func ValidSIREN(s string) bool { return len(s) == 9 && luhn(s) }

func luhn(s string) bool {
	sum := 0
	double := false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
