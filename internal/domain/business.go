// Package domain holds the core types shared by every layer of the extractor.
// It has no dependencies on infrastructure packages.
package domain

import (
	"strings"
	"time"
)

// Business is the normalized representation of a company record, whatever
// source produced it. Zero values mean "unknown".
type Business struct {
	ID       int64  `json:"id,omitempty"`
	DedupKey string `json:"dedup_key"`

	Name       string   `json:"name"`
	Category   string   `json:"category,omitempty"`
	Categories []string `json:"categories,omitempty"`

	Address    string `json:"address,omitempty"`
	Street     string `json:"street,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	City       string `json:"city,omitempty"`
	Region     string `json:"region,omitempty"`
	Country    string `json:"country,omitempty"` // ISO 3166-1 alpha-2 when known

	Lat float64 `json:"lat,omitempty"`
	Lng float64 `json:"lng,omitempty"`

	Phone   string   `json:"phone,omitempty"` // E.164 when it could be normalized
	Website string   `json:"website,omitempty"`
	Domain  string   `json:"domain,omitempty"`
	Emails  []string `json:"emails,omitempty"`
	Socials Socials  `json:"socials,omitzero"`

	Rating       float64           `json:"rating,omitempty"`
	ReviewsCount int               `json:"reviews_count,omitempty"`
	PriceLevel   string            `json:"price_level,omitempty"`
	Hours        map[string]string `json:"hours,omitempty"`
	Status       string            `json:"status,omitempty"` // OPERATIONAL | CLOSED_TEMPORARILY | CLOSED_PERMANENTLY

	// Identifiers coming from upstream sources, used for deduplication.
	PlaceID string `json:"place_id,omitempty"` // Google place id (ChIJ...)
	CID     string `json:"cid,omitempty"`      // Google customer id
	DataID  string `json:"data_id,omitempty"`  // Google Maps feature id (0x..:0x..)
	OSMID   string `json:"osm_id,omitempty"`   // node/123, way/456
	SIREN   string `json:"siren,omitempty"`
	SIRET   string `json:"siret,omitempty"`
	NAF     string `json:"naf,omitempty"` // French activity code (APE)

	Sources      []string  `json:"sources"`
	Completeness int       `json:"completeness"` // 0..100
	EnrichedAt   time.Time `json:"enriched_at,omitzero"`
	FirstSeenAt  time.Time `json:"first_seen_at,omitzero"`
	LastSeenAt   time.Time `json:"last_seen_at,omitzero"`
	SourceURL    string    `json:"source_url,omitempty"`
	Details      *Details  `json:"details,omitempty"`
	DetailsAt    time.Time `json:"details_at,omitzero"`
}

// Details is the full place profile (Google Maps place page).
type Details struct {
	Description      string                 `json:"description,omitempty"`
	Thumbnail        string                 `json:"thumbnail,omitempty"`
	Timezone         string                 `json:"timezone,omitempty"`
	PlusCode         string                 `json:"plus_code,omitempty"`
	ReviewsLink      string                 `json:"reviews_link,omitempty"`
	Owner            *Owner                 `json:"owner,omitempty"`
	Menu             *Link                  `json:"menu,omitempty"`
	Reservations     []Link                 `json:"reservations,omitempty"`
	OrderOnline      []Link                 `json:"order_online,omitempty"`
	Images           []Link                 `json:"images,omitempty"`
	Attributes       []AttributeGroup       `json:"attributes,omitempty"`
	ReviewsPerRating map[int]int            `json:"reviews_per_rating,omitempty"`
	Reviews          []Review               `json:"reviews,omitempty"`
	PopularTimes     map[string]map[int]int `json:"popular_times,omitempty"`
}

type Owner struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Link string `json:"link,omitempty"`
}

type Link struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url"`
}

// AttributeGroup is one "About" section (accessibility, payments, ...).
type AttributeGroup struct {
	ID      string      `json:"id,omitempty"`
	Name    string      `json:"name"`
	Options []Attribute `json:"options"`
}

type Attribute struct {
	Name    string   `json:"name"`
	Enabled bool     `json:"enabled"`
	Values  []string `json:"values,omitempty"`
}

type Review struct {
	ID          string    `json:"id,omitempty"`
	Author      string    `json:"author"`
	AuthorURL   string    `json:"author_url,omitempty"`
	Rating      int       `json:"rating"`
	Text        string    `json:"text,omitempty"`
	When        string    `json:"when,omitempty"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	Reply       string    `json:"owner_reply,omitempty"`
	Language    string    `json:"language,omitempty"`
}

// Socials groups the social network profiles found for a business.
type Socials struct {
	LinkedIn  string `json:"linkedin,omitempty"`
	Facebook  string `json:"facebook,omitempty"`
	Instagram string `json:"instagram,omitempty"`
	X         string `json:"x,omitempty"`
	YouTube   string `json:"youtube,omitempty"`
	TikTok    string `json:"tiktok,omitempty"`
}

// IsZero reports whether no social profile is known.
func (s Socials) IsZero() bool { return s == Socials{} }

// Merge fills empty fields of s with the values of o.
func (s *Socials) Merge(o Socials) {
	fill(&s.LinkedIn, o.LinkedIn)
	fill(&s.Facebook, o.Facebook)
	fill(&s.Instagram, o.Instagram)
	fill(&s.X, o.X)
	fill(&s.YouTube, o.YouTube)
	fill(&s.TikTok, o.TikTok)
}

const (
	StatusOperational       = "OPERATIONAL"
	StatusClosedTemporarily = "CLOSED_TEMPORARILY"
	StatusClosedPermanently = "CLOSED_PERMANENTLY"
)

// ComputeCompleteness scores how useful the record is for prospecting.
// The weights favour contact channels, which is what a CRM needs.
func (b *Business) ComputeCompleteness() int {
	score := 0
	add := func(ok bool, w int) {
		if ok {
			score += w
		}
	}
	add(b.Name != "", 10)
	add(b.Address != "" || b.Street != "", 10)
	add(b.City != "", 5)
	add(b.PostalCode != "", 5)
	add(b.Lat != 0 || b.Lng != 0, 5)
	add(b.Phone != "", 20)
	add(b.Website != "", 15)
	add(len(b.Emails) > 0, 15)
	add(b.Category != "", 5)
	add(!b.Socials.IsZero(), 5)
	add(b.SIREN != "" || b.SIRET != "", 5)
	b.Completeness = score
	return score
}

// IsComplete is the "fiche complète" threshold: identity, location and at
// least one direct contact channel.
func (b *Business) IsComplete() bool {
	return b.Name != "" && (b.Address != "" || b.City != "") && b.Phone != "" &&
		(b.Website != "" || len(b.Emails) > 0)
}

// IsCRMUseful is the "débit utile CRM" definition: an operating business
// that can actually be contacted.
func (b *Business) IsCRMUseful() bool {
	if b.Name == "" || b.Status == StatusClosedPermanently {
		return false
	}
	return b.Phone != "" || len(b.Emails) > 0 || b.Website != ""
}

// HasSource reports whether src already contributed to the record.
func (b *Business) HasSource(src string) bool {
	for _, s := range b.Sources {
		if s == src {
			return true
		}
	}
	return false
}

// Merge enriches b with the non-empty fields of o. Existing values win, except
// for list fields which are unioned and for counters where the larger wins.
func (b *Business) Merge(o *Business) {
	fill(&b.Name, o.Name)
	fill(&b.Category, o.Category)
	b.Categories = unionStrings(b.Categories, o.Categories)
	fill(&b.Address, o.Address)
	fill(&b.Street, o.Street)
	fill(&b.PostalCode, o.PostalCode)
	fill(&b.City, o.City)
	fill(&b.Region, o.Region)
	fill(&b.Country, o.Country)
	if b.Lat == 0 && b.Lng == 0 {
		b.Lat, b.Lng = o.Lat, o.Lng
	}
	fill(&b.Phone, o.Phone)
	fill(&b.Website, o.Website)
	fill(&b.Domain, o.Domain)
	b.Emails = unionStrings(b.Emails, o.Emails)
	b.Socials.Merge(o.Socials)
	if o.ReviewsCount > b.ReviewsCount {
		b.ReviewsCount = o.ReviewsCount
		b.Rating = o.Rating
	} else if b.Rating == 0 {
		b.Rating = o.Rating
	}
	fill(&b.PriceLevel, o.PriceLevel)
	if len(b.Hours) == 0 {
		b.Hours = o.Hours
	}
	fill(&b.Status, o.Status)
	fill(&b.PlaceID, o.PlaceID)
	fill(&b.CID, o.CID)
	fill(&b.DataID, o.DataID)
	if o.Details != nil && (b.Details == nil || o.DetailsAt.After(b.DetailsAt)) {
		b.Details = o.Details
	}
	if o.DetailsAt.After(b.DetailsAt) {
		b.DetailsAt = o.DetailsAt
	}
	fill(&b.OSMID, o.OSMID)
	fill(&b.SIREN, o.SIREN)
	fill(&b.SIRET, o.SIRET)
	fill(&b.NAF, o.NAF)
	fill(&b.SourceURL, o.SourceURL)
	b.Sources = unionStrings(b.Sources, o.Sources)
	if o.EnrichedAt.After(b.EnrichedAt) {
		b.EnrichedAt = o.EnrichedAt
	}
	b.ComputeCompleteness()
}

func fill(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = v
	}
}

func unionStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		out := make([]string, len(b))
		copy(out, b)
		return out
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		seen[strings.ToLower(s)] = struct{}{}
	}
	for _, s := range b {
		k := strings.ToLower(s)
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			a = append(a, s)
		}
	}
	return a
}
