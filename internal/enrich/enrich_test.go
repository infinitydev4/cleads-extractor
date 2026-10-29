package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/source"
)

const home = `<html><body>
<a href="/contact">Contactez-nous</a> <a href="/mentions-legales">Mentions légales</a> <a href="/blog">Blog</a>
<a href="https://www.facebook.com/sharer/sharer.php?u=x">share</a>
<a href="https://www.facebook.com/dupontplomberie/">FB</a>
<a href="https://fr.linkedin.com/company/dupont-plomberie">in</a>
<img src="logo@2x.png">
</body></html>`

const contact = `<p>Écrivez-nous : contact [at] dupont-plomberie [dot] fr ou <a href="mailto:devis@dupont-plomberie.fr">devis</a>
<a href="tel:+33478000000">04 78 00 00 00</a></p>`

const legal = `<p>SARL Dupont — SIRET : 732 829 320 00074 — RCS Lyon</p>`

func site(t *testing.T) (*httptest.Server, *Crawler) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/robots.txt":
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /blog\n"))
		case "/":
			_, _ = w.Write([]byte(home))
		case "/contact":
			_, _ = w.Write([]byte(contact))
		case "/mentions-legales":
			_, _ = w.Write([]byte(legal))
		case "/blog":
			t.Error("robots.txt disallowed path was crawled")
		}
	}))
	// httptest listens on loopback: use a plain client (production uses the
	// public-only client, tested separately).
	c := New(Config{HTTPClient: source.NewHTTPClient(5*time.Second, 2, nil)})
	return srv, c
}

func TestCrawl(t *testing.T) {
	srv, c := site(t)
	defer srv.Close()
	res, err := c.Crawl(context.Background(), srv.URL, "FR")
	if err != nil {
		t.Fatal(err)
	}
	p := res.Patch
	if res.Pages != 3 {
		t.Errorf("pages = %d, want 3", res.Pages)
	}
	if len(p.Emails) != 2 {
		t.Errorf("emails = %v (want obfuscated contact@ + mailto devis@)", p.Emails)
	}
	if p.Phone != "+33478000000" {
		t.Errorf("phone = %q", p.Phone)
	}
	if p.SIRET != "73282932000074" || p.SIREN != "732829320" {
		t.Errorf("siret = %q siren = %q", p.SIRET, p.SIREN)
	}
	if p.Socials.Facebook != "https://www.facebook.com/dupontplomberie" || !strings.Contains(p.Socials.LinkedIn, "company/dupont-plomberie") {
		t.Errorf("socials = %+v", p.Socials)
	}
	if p.EnrichedAt.IsZero() {
		t.Error("EnrichedAt not set")
	}
}

func TestPublicClientRefusesPrivateNetworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := New(Config{}) // default = public-only client
	_, err := c.Crawl(context.Background(), srv.URL, "FR")
	var ue *url.Error
	if err == nil || !errors.As(err, &ue) || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("loopback must be refused, got %v", err)
	}
}

func TestParseRobots(t *testing.T) {
	r := ParseRobots([]byte("User-agent: Googlebot\nDisallow: /\n\nUser-agent: *\nDisallow: /private\nAllow: /private/ok\n"), "cleads-enricher")
	cases := map[string]bool{"/": true, "/private/x": false, "/private/ok/page": true, "/contact": true}
	for p, want := range cases {
		if got := r.allowed(p); got != want {
			t.Errorf("allowed(%q) = %v", p, got)
		}
	}
	specific := ParseRobots([]byte("User-agent: cleads-enricher\nDisallow: /\n"), "cleads-enricher")
	if specific.allowed("/contact") {
		t.Error("agent-specific disallow ignored")
	}
}

func TestSortEmails(t *testing.T) {
	es := []string{"jean@gmail.com", "zz@dupont.fr", "contact@dupont.fr"}
	sortEmails(es, "dupont.fr")
	if es[0] != "contact@dupont.fr" || es[2] != "jean@gmail.com" {
		t.Fatal(es)
	}
}

func TestSelectEmails(t *testing.T) {
	got := selectEmails([]string{"developer@udevweb.co", "privacy@urecommend.co", "resa@clos.fr", "clos.jouve@gmail.com"}, "clos.fr")
	if len(got) != 2 || got[0] != "resa@clos.fr" {
		t.Fatalf("own-domain and free-mail addresses must win: %v", got)
	}
	got = selectEmails([]string{"developer@udevweb.co", "privacy@urecommend.co", "groupe@holding-resto.fr"}, "clos.fr")
	if len(got) != 1 || got[0] != "groupe@holding-resto.fr" {
		t.Fatalf("technical third-party boxes must be dropped: %v", got)
	}
}
