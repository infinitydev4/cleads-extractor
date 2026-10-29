// Package placesapi implements the official Google Places API (New) Text
// Search source. It is the recommended Google source: stable, contractual,
// and billed per request (field masks drive the SKU, so we request only what
// the CRM uses).
package placesapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
)

const (
	Name            = "google_places"
	defaultEndpoint = "https://places.googleapis.com/v1/places:searchText"
	pageSize        = 20
	maxPages        = 3 // Text Search returns at most 60 results per query
)

// Field masks. "basic" avoids the contact-data SKU (no phone/website).
var fieldMasks = map[string]string{
	"basic": "places.id,places.displayName,places.formattedAddress,places.addressComponents,places.location," +
		"places.businessStatus,places.types,places.primaryTypeDisplayName,places.googleMapsUri,nextPageToken",
	"contact": "places.id,places.displayName,places.formattedAddress,places.addressComponents,places.location," +
		"places.businessStatus,places.types,places.primaryTypeDisplayName,places.googleMapsUri," +
		"places.nationalPhoneNumber,places.internationalPhoneNumber,places.websiteUri,places.rating," +
		"places.userRatingCount,places.priceLevel,places.regularOpeningHours.weekdayDescriptions,nextPageToken",
}

// Config of the source.
type Config struct {
	APIKey      string
	FieldTier   string // basic | contact
	Endpoint    string
	HTTPClient  *http.Client
	RatePerSec  float64
	Concurrency int
}

type Source struct {
	cfg  Config
	mask string
}

func New(cfg Config) (*Source, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("%s: API key is required", Name)
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.FieldTier == "" {
		cfg.FieldTier = "contact"
	}
	mask, ok := fieldMasks[cfg.FieldTier]
	if !ok {
		return nil, fmt.Errorf("%s: unknown field tier %q", Name, cfg.FieldTier)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = source.NewHTTPClient(15*time.Second, 64, nil)
	}
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec = 8
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 16
	}
	return &Source{cfg: cfg, mask: mask}, nil
}

func (s *Source) Info() source.Info {
	return source.Info{
		Name:          Name,
		Description:   "Google Places API (New) Text Search — official, billed per request, max 60 results per query.",
		Official:      true,
		DefaultCellKm: 3,
		MaxCells:      400,
		MinCellKm:     0.4,
		RatePerSec:    s.cfg.RatePerSec,
		Burst:         int(s.cfg.RatePerSec) + 1,
		Concurrency:   s.cfg.Concurrency,
	}
}

type latLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type searchBody struct {
	TextQuery           string `json:"textQuery"`
	LanguageCode        string `json:"languageCode,omitempty"`
	RegionCode          string `json:"regionCode,omitempty"`
	PageSize            int    `json:"pageSize"`
	PageToken           string `json:"pageToken,omitempty"`
	IncludedType        string `json:"includedType,omitempty"`
	StrictTypeFiltering bool   `json:"strictTypeFiltering,omitempty"`
	LocationRestriction *struct {
		Rectangle struct {
			Low  latLng `json:"low"`
			High latLng `json:"high"`
		} `json:"rectangle"`
	} `json:"locationRestriction,omitempty"`
}

type place struct {
	ID          string `json:"id"`
	DisplayName struct {
		Text string `json:"text"`
	} `json:"displayName"`
	FormattedAddress  string `json:"formattedAddress"`
	AddressComponents []struct {
		LongText  string   `json:"longText"`
		ShortText string   `json:"shortText"`
		Types     []string `json:"types"`
	} `json:"addressComponents"`
	Location                 latLng                `json:"location"`
	BusinessStatus           string                `json:"businessStatus"`
	Types                    []string              `json:"types"`
	PrimaryTypeDisplayName   struct{ Text string } `json:"primaryTypeDisplayName"`
	GoogleMapsURI            string                `json:"googleMapsUri"`
	NationalPhoneNumber      string                `json:"nationalPhoneNumber"`
	InternationalPhoneNumber string                `json:"internationalPhoneNumber"`
	WebsiteURI               string                `json:"websiteUri"`
	Rating                   float64               `json:"rating"`
	UserRatingCount          int                   `json:"userRatingCount"`
	PriceLevel               string                `json:"priceLevel"`
	RegularOpeningHours      struct {
		WeekdayDescriptions []string `json:"weekdayDescriptions"`
	} `json:"regularOpeningHours"`
}

type searchResponse struct {
	Places        []place `json:"places"`
	NextPageToken string  `json:"nextPageToken"`
}

func (s *Source) Search(ctx context.Context, req source.Request) (source.Result, error) {
	body := searchBody{
		TextQuery:    req.SearchText(),
		LanguageCode: req.Language,
		RegionCode:   req.Country,
		PageSize:     pageSize,
		PageToken:    req.PageToken,
	}
	if body.TextQuery == "" {
		return source.Result{}, fmt.Errorf("%w: empty query", source.ErrPermanent)
	}
	if req.Sector != nil && req.Sector.GoogleType != "" && req.Query == "" {
		body.IncludedType = req.Sector.GoogleType
		body.StrictTypeFiltering = true
	}
	if req.Cell.Valid() {
		body.LocationRestriction = &struct {
			Rectangle struct {
				Low  latLng `json:"low"`
				High latLng `json:"high"`
			} `json:"rectangle"`
		}{}
		body.LocationRestriction.Rectangle.Low = latLng{req.Cell.MinLat, req.Cell.MinLng}
		body.LocationRestriction.Rectangle.High = latLng{req.Cell.MaxLat, req.Cell.MaxLng}
	} else if req.Location != "" {
		body.TextQuery += " " + req.Location
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return source.Result{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return source.Result{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-Goog-Api-Key", s.cfg.APIKey)
	hreq.Header.Set("X-Goog-FieldMask", s.mask)

	resp, err := s.cfg.HTTPClient.Do(hreq)
	if err != nil {
		return source.Result{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return source.Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return source.Result{}, source.ClassifyHTTP(resp, raw)
	}
	var sr searchResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return source.Result{}, fmt.Errorf("decode places response: %w", err)
	}

	out := source.Result{Businesses: make([]domain.Business, 0, len(sr.Places))}
	for i := range sr.Places {
		out.Businesses = append(out.Businesses, toBusiness(&sr.Places[i], req.Country))
	}
	switch {
	case sr.NextPageToken != "" && req.Page+1 < maxPages:
		out.NextPageToken = sr.NextPageToken
	case req.Page+1 >= maxPages && len(sr.Places) >= pageSize:
		// 60 results reached: there are probably more in this cell.
		out.Saturated = true
	}
	return out, nil
}

func toBusiness(p *place, country string) domain.Business {
	b := domain.Business{
		Name:         p.DisplayName.Text,
		Category:     p.PrimaryTypeDisplayName.Text,
		Categories:   p.Types,
		Address:      p.FormattedAddress,
		Lat:          p.Location.Latitude,
		Lng:          p.Location.Longitude,
		Phone:        firstNonEmpty(p.InternationalPhoneNumber, p.NationalPhoneNumber),
		Website:      p.WebsiteURI,
		Rating:       p.Rating,
		ReviewsCount: p.UserRatingCount,
		PriceLevel:   p.PriceLevel,
		Status:       p.BusinessStatus,
		PlaceID:      p.ID,
		SourceURL:    p.GoogleMapsURI,
		Sources:      []string{Name},
		Country:      country,
	}
	var streetNumber, route string
	for _, c := range p.AddressComponents {
		for _, t := range c.Types {
			switch t {
			case "street_number":
				streetNumber = c.LongText
			case "route":
				route = c.LongText
			case "postal_code":
				b.PostalCode = c.LongText
			case "locality":
				b.City = c.LongText
			case "postal_town":
				if b.City == "" {
					b.City = c.LongText
				}
			case "administrative_area_level_1":
				b.Region = c.LongText
			case "country":
				b.Country = c.ShortText
			}
		}
	}
	b.Street = strings.TrimSpace(streetNumber + " " + route)
	if n := len(p.RegularOpeningHours.WeekdayDescriptions); n > 0 {
		b.Hours = make(map[string]string, n)
		for _, d := range p.RegularOpeningHours.WeekdayDescriptions {
			if day, hours, ok := strings.Cut(d, ": "); ok {
				b.Hours[day] = hours
			}
		}
	}
	if u, err := url.Parse(p.GoogleMapsURI); err == nil {
		b.CID = u.Query().Get("cid")
	}
	return b
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
