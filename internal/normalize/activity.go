package normalize

import (
	"strings"
	"unicode"
)

// Generic words that say nothing about the activity itself.
var activityStopwords = map[string]struct{}{
	"agence": {}, "salon": {}, "magasin": {}, "cabinet": {}, "service": {}, "services": {}, "entreprise": {},
	"societe": {}, "centre": {}, "atelier": {}, "boutique": {}, "institut": {}, "maison": {}, "groupe": {},
	"store": {}, "shop": {}, "company": {}, "de": {}, "du": {}, "la": {}, "le": {}, "les": {}, "des": {},
	"et": {}, "en": {}, "au": {}, "aux": {}, "pour": {}, "the": {}, "and": {}, "of": {},
}

const stemLen = 4

// ActivityStems turns activity descriptions ("plombier", "salon de
// coiffure") into comparison stems: folded, stop words removed, truncated to
// 4 runes so that inflections match ("coiffeur" ~ "coiffure", "plombier" ~
// "plomberie", "dentiste" ~ "dentaire").
func ActivityStems(texts ...string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, t := range texts {
		for _, w := range strings.FieldsFunc(Fold(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if _, stop := activityStopwords[w]; stop || len([]rune(w)) < stemLen {
				continue
			}
			s := string([]rune(w)[:stemLen])
			if _, dup := seen[s]; !dup {
				seen[s] = struct{}{}
				out = append(out, s)
			}
		}
	}
	return out
}

// MatchesActivity reports whether a business looks like the searched
// activity, from its categories or its name. Empty stems match everything.
func MatchesActivity(stems []string, name string, categories []string) bool {
	if len(stems) == 0 {
		return true
	}
	fields := append([]string{name}, categories...)
	for _, f := range fields {
		for _, w := range strings.FieldsFunc(Fold(f), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			for _, s := range stems {
				if strings.HasPrefix(w, s) {
					return true
				}
			}
		}
	}
	return false
}
