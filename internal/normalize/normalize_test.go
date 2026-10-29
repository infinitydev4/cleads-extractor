package normalize

import (
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
)

func TestPhone(t *testing.T) {
	cases := []struct{ in, country, want string }{
		{"01 23 45 67 89", "FR", "+33123456789"},
		{"01.23.45.67.89", "FR", "+33123456789"},
		{"+33 1 23 45 67 89", "FR", "+33123456789"},
		{"+33 (0)1 23 45 67 89", "FR", "+33123456789"},
		{"0033 6 12 34 56 78", "FR", "+33612345678"},
		{"33612345678", "FR", "+33612345678"},
		{"06 12 34 56 78 / 07 00 00 00 00", "FR", "+33612345678"},
		{"(415) 555-0123", "US", "+14155550123"},
		{"1-415-555-0123", "US", "+14155550123"},
		{"02 123 45 67", "BE", "+3221234567"},
		{"25101555", "CY", "25101555"}, // unknown plan: digits kept
		{"abc", "FR", ""},
		{"", "FR", ""},
		{"01 23 45 67 89 poste 12", "FR", "+33123456789"},
		{"0123456789x12", "FR", "+33123456789"},
	}
	for _, c := range cases {
		if got := Phone(c.in, c.country); got != c.want {
			t.Errorf("Phone(%q, %s) = %q, want %q", c.in, c.country, got, c.want)
		}
	}
}

func TestWebsite(t *testing.T) {
	cases := []struct{ in, url, host, social string }{
		{"www.Example.FR", "https://www.example.fr", "example.fr", ""},
		{"https://example.fr/?utm_source=gmb&x=1", "https://example.fr?x=1", "example.fr", ""},
		{"http://example.fr:80/contact#top", "http://example.fr/contact", "example.fr", ""},
		{"https://www.facebook.com/plomberie.dupont", "https://www.facebook.com/plomberie.dupont", "facebook.com", "facebook"},
		{"http://example.fr:8080/a", "http://example.fr:8080/a", "example.fr", ""},
		{"ftp://example.fr", "", "", ""},
		{"localhost", "", "", ""},
		{"", "", "", ""},
	}
	for _, c := range cases {
		u, h, s := Website(c.in)
		if u != c.url || h != c.host || s != c.social {
			t.Errorf("Website(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, u, h, s, c.url, c.host, c.social)
		}
	}
}

func TestEmail(t *testing.T) {
	ok := map[string]string{
		"Contact@Dupont-Plomberie.FR":  "contact@dupont-plomberie.fr",
		"mailto:info@ex.fr?subject=hi": "info@ex.fr",
		"<jean.dupont@orange.fr>":      "jean.dupont@orange.fr",
	}
	for in, want := range ok {
		if got := Email(in); got != want {
			t.Errorf("Email(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"logo@2x.png", "user@example.com", "noat.fr", "a@b", "x@sentry.wixpress.com", "é@ex.fr", "you@company.com", "nom@exemple.fr", "email@dupont.fr"} {
		if got := Email(bad); got != "" {
			t.Errorf("Email(%q) = %q, want empty", bad, got)
		}
	}
}

func TestNameKey(t *testing.T) {
	cases := map[string]string{
		"SARL Dupont Plomberie":     "dupont plomberie",
		"Dupont-Plomberie":          "dupont plomberie",
		"  DUPONT   PLOMBERIE SAS ": "dupont plomberie",
		"Café de l'Étoile":          "cafe etoile",
		"Les Délices":               "delices",
		"SAS":                       "sas",
	}
	for in, want := range cases {
		if got := NameKey(in); got != want {
			t.Errorf("NameKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSIRET(t *testing.T) {
	if !ValidSIRET("73282932000074") { // Apple France
		t.Error("valid SIRET rejected")
	}
	if ValidSIRET("73282932000075") {
		t.Error("invalid SIRET accepted")
	}
	if !ValidSIREN("732829320") {
		t.Error("valid SIREN rejected")
	}
	if !ValidSIRET("35600000000051") { // La Poste special rule
		t.Error("La Poste SIRET rejected")
	}
}

func TestPostalCodeFR(t *testing.T) {
	cases := map[string]string{
		"12 rue de la Paix, 75002 Paris": "75002",
		"Lyon 69003":                     "69003",
		"Tel 0123456789":                 "",
		"BP 123":                         "",
	}
	for in, want := range cases {
		if got := PostalCodeFR(in); got != want {
			t.Errorf("PostalCodeFR(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBusiness(t *testing.T) {
	b := domain.Business{
		Name: "  Dupont   Plomberie ", Phone: "04 78 00 00 00", Website: "https://facebook.com/dupont",
		Address: "1 rue X, 69003 Lyon", Emails: []string{"A@dupont.fr", "a@dupont.fr", "bad"},
		SIRET: "12345678901234",
	}
	Business(&b, "FR")
	if b.Name != "Dupont Plomberie" || b.Phone != "+33478000000" || b.Website != "" ||
		b.Socials.Facebook == "" || b.PostalCode != "69003" || len(b.Emails) != 1 || b.SIRET != "" {
		t.Fatalf("unexpected normalization: %+v", b)
	}
	if b.Completeness == 0 {
		t.Fatal("completeness not computed")
	}
}

func BenchmarkNameKey(b *testing.B) {
	for b.Loop() {
		NameKey("SARL Plomberie Chauffage Dupont & Fils - Lyon 3e")
	}
}

func BenchmarkPhone(b *testing.B) {
	for b.Loop() {
		Phone("+33 (0)4 78 12 34 56", "FR")
	}
}
