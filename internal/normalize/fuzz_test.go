package normalize

import "testing"

func FuzzNormalizers(f *testing.F) {
	for _, s := range []string{"+33 (0)1 23 45 67 89", "0033612345678", "https://www.Example.fr/?utm_x=1", "contact@x.fr", "SARL Café", "75002 Paris", "+", "00", "+3300", "330"} {
		f.Add(s, "FR")
		f.Add(s, "US")
	}
	f.Fuzz(func(t *testing.T, s, country string) {
		_ = Phone(s, country)
		_, _, _ = Website(s)
		_ = Email(s)
		_ = NameKey(s)
		_ = PostalCodeFR(s)
		_ = ValidSIRET(s)
	})
}
