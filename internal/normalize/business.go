package normalize

import (
	"strings"

	"github.com/cleads/business-extractor/internal/domain"
)

// Business canonicalizes every field of b in place. country is the fallback
// ISO country used to interpret national phone numbers.
func Business(b *domain.Business, country string) {
	b.Name = strings.Join(strings.Fields(b.Name), " ")
	if b.Country == "" {
		b.Country = strings.ToUpper(country)
	}
	b.Phone = Phone(b.Phone, b.Country)
	if b.Website != "" {
		canon, host, social := Website(b.Website)
		switch social {
		case "":
			b.Website, b.Domain = canon, host
		default:
			// A Facebook page is not a website: file it as a social profile.
			setSocial(&b.Socials, social, canon)
			b.Website, b.Domain = "", ""
		}
	}
	if len(b.Emails) > 0 {
		out := b.Emails[:0]
		seen := make(map[string]struct{}, len(b.Emails))
		for _, e := range b.Emails {
			if n := Email(e); n != "" {
				if _, dup := seen[n]; !dup {
					seen[n] = struct{}{}
					out = append(out, n)
				}
			}
		}
		b.Emails = out
	}
	if b.PostalCode == "" && b.Country == "FR" {
		b.PostalCode = PostalCodeFR(b.Address)
	}
	if b.Address == "" && (b.Street != "" || b.City != "") {
		b.Address = strings.Trim(strings.TrimSpace(b.Street+", "+strings.TrimSpace(b.PostalCode+" "+b.City)), ", ")
	}
	if b.SIRET != "" && !ValidSIRET(b.SIRET) {
		b.SIRET = ""
	}
	if b.SIREN == "" && len(b.SIRET) == 14 {
		b.SIREN = b.SIRET[:9]
	}
	if b.Category == "" && len(b.Categories) > 0 {
		b.Category = b.Categories[0]
	}
	b.ComputeCompleteness()
}

func setSocial(s *domain.Socials, kind, u string) {
	switch kind {
	case "facebook":
		if s.Facebook == "" {
			s.Facebook = u
		}
	case "instagram":
		if s.Instagram == "" {
			s.Instagram = u
		}
	case "linkedin":
		if s.LinkedIn == "" {
			s.LinkedIn = u
		}
	case "x":
		if s.X == "" {
			s.X = u
		}
	case "youtube":
		if s.YouTube == "" {
			s.YouTube = u
		}
	case "tiktok":
		if s.TikTok == "" {
			s.TikTok = u
		}
	}
}
