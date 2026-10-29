// Package enrich crawls a business website (home + a few contact / legal
// pages) to extract emails, phones, social profiles and, for French sites,
// the SIRET published in the "mentions légales".
//
// It is deliberately frugal: at most MaxPages pages, a body cap, strict
// timeouts, one in-flight request per host and robots.txt honoured.
package enrich

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/normalize"
	"github.com/cleads/business-extractor/internal/source"
)

const userAgent = "CLEADS-Enricher/1.0 (+https://cleads.fr/bot)"

type Config struct {
	MaxPages    int
	MaxBodySize int64
	Timeout     time.Duration
	HTTPClient  *http.Client
}

type Crawler struct {
	cfg    Config
	robots sync.Map // host -> *robotsRules
	hosts  sync.Map // host -> chan struct{} (1 slot)
}

func New(cfg Config) *Crawler {
	if cfg.MaxPages == 0 {
		cfg.MaxPages = 4
	}
	if cfg.MaxBodySize == 0 {
		cfg.MaxBodySize = 1 << 20
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 8 * time.Second
	}
	if cfg.HTTPClient == nil {
		c := source.NewPublicHTTPClient(cfg.Timeout, 2)
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		}
		cfg.HTTPClient = c
	}
	return &Crawler{cfg: cfg}
}

var (
	emailRe   = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,24}`)
	obfusRe   = regexp.MustCompile(`(?i)([a-z0-9._%+\-]+)\s*[\[\(\{]\s*(?:at|arobase|@)\s*[\]\)\}]\s*([a-z0-9\-]+)\s*[\[\(\{]\s*(?:dot|point|\.)\s*[\]\)\}]\s*([a-z]{2,24})`)
	telRe     = regexp.MustCompile(`(?i)href=["']tel:([+0-9 ().\-]{6,25})["']`)
	frPhoneRe = regexp.MustCompile(`(?:\+33\s?[1-9]|\b0[1-9])(?:[\s.\-]?\d{2}){4}\b`)
	siretRe   = regexp.MustCompile(`(?i)siret\D{0,20}((?:\d[\s.]?){14})`)
	sirenRe   = regexp.MustCompile(`(?i)(?:siren|rcs[^0-9]{0,40})\D{0,10}((?:\d[\s.]?){9})\b`)
	hrefRe    = regexp.MustCompile(`(?i)href=["']([^"'#]+)["']`)
	socialRe  = map[string]*regexp.Regexp{
		"linkedin":  regexp.MustCompile(`(?i)https?://(?:[a-z]{2,3}\.)?linkedin\.com/(?:company|in|school)/[^\s"'<>?#]+`),
		"facebook":  regexp.MustCompile(`(?i)https?://(?:www\.|m\.)?facebook\.com/(?:[^\s"'<>?#/]+)/?`),
		"instagram": regexp.MustCompile(`(?i)https?://(?:www\.)?instagram\.com/[^\s"'<>?#/]+/?`),
		"x":         regexp.MustCompile(`(?i)https?://(?:www\.)?(?:twitter|x)\.com/[A-Za-z0-9_]{1,15}\b`),
		"youtube":   regexp.MustCompile(`(?i)https?://(?:www\.)?youtube\.com/(?:@|channel/|c/|user/)[^\s"'<>?#]+`),
		"tiktok":    regexp.MustCompile(`(?i)https?://(?:www\.)?tiktok\.com/@[^\s"'<>?#/]+`),
	}
	contactHints = []string{"contact", "mentions", "legal", "a-propos", "about", "qui-sommes", "impressum", "nous-contacter", "cgv"}
	socialNoise  = []string{"/sharer", "/share", "/plugins", "/tr?", "/dialog", "/intent", "facebook.com/pages/category", "/hashtag/"}
)

// Result of a crawl.
type Result struct {
	Patch domain.Business
	Pages int
}

// Crawl visits the site and returns a patch to merge into the business.
func (c *Crawler) Crawl(ctx context.Context, website, country string) (Result, error) {
	canon, host, social := normalize.Website(website)
	if canon == "" || social != "" {
		return Result{}, fmt.Errorf("not a crawlable website: %q", website)
	}
	base, err := url.Parse(canon)
	if err != nil {
		return Result{}, err
	}
	release, err := c.acquireHost(ctx, host)
	if err != nil {
		return Result{}, err
	}
	defer release()

	rules := c.robotsFor(ctx, base)
	var (
		res     Result
		emails  = map[string]struct{}{}
		visited = map[string]struct{}{}
		queue   = []string{base.String()}
	)
	res.Patch.EnrichedAt = time.Now().UTC()
	for len(queue) > 0 && res.Pages < c.cfg.MaxPages {
		u := queue[0]
		queue = queue[1:]
		if _, ok := visited[u]; ok {
			continue
		}
		visited[u] = struct{}{}
		pu, err := url.Parse(u)
		if err != nil || !rules.allowed(pu.EscapedPath()) {
			continue
		}
		body, finalURL, err := c.fetch(ctx, u)
		if err != nil {
			if res.Pages == 0 {
				return res, err
			}
			continue
		}
		res.Pages++
		c.extract(body, country, &res.Patch, emails)
		if res.Pages == 1 {
			// discover contact/legal pages from the home page only
			fu, _ := url.Parse(finalURL)
			if fu == nil {
				fu = base
			}
			queue = append(queue, contactLinks(body, fu, host)...)
		}
	}
	all := make([]string, 0, len(emails))
	for e := range emails {
		all = append(all, e)
	}
	res.Patch.Emails = selectEmails(all, host)
	return res, nil
}

func (c *Crawler) acquireHost(ctx context.Context, host string) (func(), error) {
	v, _ := c.hosts.LoadOrStore(host, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Crawler) fetch(ctx context.Context, u string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "fr-FR,fr;q=0.9,en;q=0.6")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "text") {
		return nil, "", fmt.Errorf("unsupported content type %q", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodySize))
	return body, resp.Request.URL.String(), err
}

func (c *Crawler) extract(body []byte, country string, p *domain.Business, emails map[string]struct{}) {
	for _, m := range emailRe.FindAll(body, 50) {
		if e := normalize.Email(string(m)); e != "" {
			emails[e] = struct{}{}
		}
	}
	for _, m := range obfusRe.FindAllSubmatch(body, 10) {
		if e := normalize.Email(string(m[1]) + "@" + string(m[2]) + "." + string(m[3])); e != "" {
			emails[e] = struct{}{}
		}
	}
	if p.Phone == "" {
		if m := telRe.FindSubmatch(body); m != nil {
			p.Phone = normalize.Phone(string(m[1]), country)
		} else if country == "FR" {
			if m := frPhoneRe.Find(body); m != nil {
				p.Phone = normalize.Phone(string(m), country)
			}
		}
	}
	for name, re := range socialRe {
		for _, m := range re.FindAll(body, 5) {
			u := string(m)
			if noisy(u) {
				continue
			}
			setSocial(&p.Socials, name, strings.TrimRight(u, "/"))
			break
		}
	}
	if p.SIRET == "" {
		for _, m := range siretRe.FindAllSubmatch(body, 5) {
			d := digits(m[1])
			if normalize.ValidSIRET(d) {
				p.SIRET, p.SIREN = d, d[:9]
				break
			}
		}
	}
	if p.SIREN == "" {
		for _, m := range sirenRe.FindAllSubmatch(body, 5) {
			if d := digits(m[1]); normalize.ValidSIREN(d) {
				p.SIREN = d
				break
			}
		}
	}
}

func setSocial(s *domain.Socials, name, u string) {
	var dst *string
	switch name {
	case "linkedin":
		dst = &s.LinkedIn
	case "facebook":
		dst = &s.Facebook
	case "instagram":
		dst = &s.Instagram
	case "x":
		dst = &s.X
	case "youtube":
		dst = &s.YouTube
	case "tiktok":
		dst = &s.TikTok
	}
	if dst != nil && *dst == "" {
		*dst = u
	}
}

func noisy(u string) bool {
	for _, n := range socialNoise {
		if strings.Contains(u, n) {
			return true
		}
	}
	return false
}

func digits(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}

// contactLinks returns same-host links that look like contact/legal pages.
func contactLinks(body []byte, base *url.URL, host string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, m := range hrefRe.FindAllSubmatch(body, 500) {
		href := string(bytes.TrimSpace(m[1]))
		lh := strings.ToLower(href)
		if strings.HasPrefix(lh, "mailto:") || strings.HasPrefix(lh, "tel:") || strings.HasPrefix(lh, "javascript:") {
			continue
		}
		hit := false
		for _, h := range contactHints {
			if strings.Contains(lh, h) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		u, err := base.Parse(href)
		if err != nil || strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") != host {
			continue
		}
		u.Fragment = ""
		s := u.String()
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) >= 6 {
			break
		}
	}
	return out
}

var freeMailDomains = map[string]struct{}{
	"gmail.com": {}, "orange.fr": {}, "wanadoo.fr": {}, "free.fr": {}, "sfr.fr": {}, "neuf.fr": {},
	"hotmail.com": {}, "hotmail.fr": {}, "outlook.com": {}, "outlook.fr": {}, "live.fr": {}, "live.com": {},
	"yahoo.fr": {}, "yahoo.com": {}, "laposte.net": {}, "icloud.com": {}, "me.com": {}, "aol.com": {},
	"bbox.fr": {}, "numericable.fr": {}, "protonmail.com": {}, "proton.me": {}, "gmx.fr": {}, "msn.com": {},
}

// Technical mailboxes that belong to whoever built or hosts the site.
var technicalLocals = []string{"privacy", "developer", "dev", "webmaster", "dpo", "abuse", "noreply", "no-reply",
	"postmaster", "hostmaster", "support", "admin", "wordpress", "security", "gdpr", "rgpd"}

// selectEmails keeps the addresses that plausibly belong to the business:
// those on the site's domain or on consumer mail providers. Third-party
// addresses (web agency, theme author, analytics) are kept only when nothing
// better was found, and never technical mailboxes. Result is ranked: own
// domain first, then generic contact boxes.
func selectEmails(es []string, host string) []string {
	var own, other []string
	for _, e := range es {
		local, dom, _ := strings.Cut(e, "@")
		_, free := freeMailDomains[dom]
		switch {
		case dom == host || strings.HasSuffix(dom, "."+host) || strings.HasSuffix(host, "."+dom) || free:
			own = append(own, e)
		default:
			technical := false
			for _, t := range technicalLocals {
				if local == t || strings.HasPrefix(local, t+".") {
					technical = true
					break
				}
			}
			if !technical {
				other = append(other, e)
			}
		}
	}
	out := own
	if len(out) == 0 {
		out = other
	}
	if len(out) > 5 {
		out = out[:5]
	}
	sortEmails(out, host)
	return out
}

// sortEmails puts addresses on the site's own domain first, then generic
// mailboxes (contact@, info@) before personal ones.
func sortEmails(es []string, host string) {
	score := func(e string) int {
		s := 0
		if strings.HasSuffix(e, "@"+host) {
			s -= 10
		}
		for _, p := range []string{"contact@", "info@", "accueil@", "bonjour@", "hello@"} {
			if strings.HasPrefix(e, p) {
				s -= 5
			}
		}
		return s
	}
	sort.SliceStable(es, func(i, j int) bool {
		if score(es[i]) != score(es[j]) {
			return score(es[i]) < score(es[j])
		}
		return es[i] < es[j]
	})
}

// ---------------------------------------------------------------- robots.txt

type robotsRules struct {
	disallow []string
	allow    []string
}

func (r *robotsRules) allowed(path string) bool {
	if r == nil {
		return true
	}
	if path == "" {
		path = "/"
	}
	best, verdict := -1, true
	for _, a := range r.allow {
		if strings.HasPrefix(path, a) && len(a) > best {
			best, verdict = len(a), true
		}
	}
	for _, d := range r.disallow {
		if d != "" && strings.HasPrefix(path, d) && len(d) > best {
			best, verdict = len(d), false
		}
	}
	return verdict
}

func (c *Crawler) robotsFor(ctx context.Context, base *url.URL) *robotsRules {
	key := base.Scheme + "://" + base.Host
	if v, ok := c.robots.Load(key); ok {
		return v.(*robotsRules)
	}
	body, _, err := c.fetchRaw(ctx, key+"/robots.txt")
	var rules *robotsRules
	if err == nil {
		rules = ParseRobots(body, "cleads-enricher")
	}
	c.robots.Store(key, rules)
	return rules
}

func (c *Crawler) fetchRaw(ctx context.Context, u string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return b, u, err
}

// ParseRobots extracts the rules applying to agent (or "*").
func ParseRobots(body []byte, agent string) *robotsRules {
	agent = strings.ToLower(agent)
	var specific, generic robotsRules
	var current []*robotsRules
	inGroupHeader := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if !inGroupHeader {
				current = current[:0]
			}
			inGroupHeader = true
			ua := strings.ToLower(v)
			if ua == "*" {
				current = append(current, &generic)
			} else if strings.Contains(agent, ua) {
				current = append(current, &specific)
			}
		case "disallow", "allow":
			inGroupHeader = false
			for _, r := range current {
				if k == "disallow" {
					r.disallow = append(r.disallow, v)
				} else {
					r.allow = append(r.allow, v)
				}
			}
		default:
			inGroupHeader = false
		}
	}
	if len(specific.disallow)+len(specific.allow) > 0 {
		return &specific
	}
	return &generic
}
