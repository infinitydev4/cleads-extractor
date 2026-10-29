// Package taxonomy maps business sectors to the native classification of each
// source: Google Places types, OpenStreetMap tags and French NAF (APE) codes.
// Using native filters instead of free text improves precision and reduces
// wasted requests.
package taxonomy

import (
	"sort"
	"strings"
)

// Sector is one activity of the taxonomy.
type Sector struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`       // French query text
	GoogleType string   `json:"google_type"` // Places API (New) includedType
	OSMTags    []string `json:"osm_tags"`    // key=value
	NAF        []string `json:"naf"`         // NAF rév. 2 codes (with dot), e.g. 43.22A
	Synonyms   []string `json:"synonyms,omitempty"`
}

var sectors = []Sector{
	{Key: "plumber", Label: "plombier", GoogleType: "plumber", OSMTags: []string{"craft=plumber"}, NAF: []string{"43.22A"}, Synonyms: []string{"plomberie", "chauffagiste"}},
	{Key: "electrician", Label: "électricien", GoogleType: "electrician", OSMTags: []string{"craft=electrician"}, NAF: []string{"43.21A"}, Synonyms: []string{"electricite"}},
	{Key: "roofer", Label: "couvreur", GoogleType: "roofing_contractor", OSMTags: []string{"craft=roofer"}, NAF: []string{"43.91B"}, Synonyms: []string{"toiture"}},
	{Key: "painter", Label: "peintre en bâtiment", GoogleType: "painter", OSMTags: []string{"craft=painter"}, NAF: []string{"43.34Z"}},
	{Key: "carpenter", Label: "menuisier", GoogleType: "general_contractor", OSMTags: []string{"craft=carpenter"}, NAF: []string{"43.32A", "16.23Z"}, Synonyms: []string{"menuiserie"}},
	{Key: "mason", Label: "maçon", GoogleType: "general_contractor", OSMTags: []string{"craft=builder"}, NAF: []string{"43.99C"}, Synonyms: []string{"maconnerie"}},
	{Key: "locksmith", Label: "serrurier", GoogleType: "locksmith", OSMTags: []string{"craft=locksmith", "shop=locksmith"}, NAF: []string{"43.32B"}},
	{Key: "hvac", Label: "climatisation chauffage", GoogleType: "hvac_contractor", OSMTags: []string{"craft=hvac"}, NAF: []string{"43.22B"}},
	{Key: "restaurant", Label: "restaurant", GoogleType: "restaurant", OSMTags: []string{"amenity=restaurant"}, NAF: []string{"56.10A"}},
	{Key: "fast_food", Label: "restauration rapide", GoogleType: "fast_food_restaurant", OSMTags: []string{"amenity=fast_food"}, NAF: []string{"56.10C"}},
	{Key: "cafe", Label: "café bar", GoogleType: "cafe", OSMTags: []string{"amenity=cafe", "amenity=bar"}, NAF: []string{"56.30Z"}},
	{Key: "bakery", Label: "boulangerie", GoogleType: "bakery", OSMTags: []string{"shop=bakery"}, NAF: []string{"10.71C", "10.71D"}, Synonyms: []string{"patisserie", "viennoiserie"}},
	{Key: "butcher", Label: "boucherie", GoogleType: "butcher_shop", OSMTags: []string{"shop=butcher"}, NAF: []string{"47.22Z"}},
	{Key: "hairdresser", Label: "coiffeur", GoogleType: "hair_salon", OSMTags: []string{"shop=hairdresser"}, NAF: []string{"96.02A"}, Synonyms: []string{"salon de coiffure"}},
	{Key: "beauty", Label: "institut de beauté", GoogleType: "beauty_salon", OSMTags: []string{"shop=beauty"}, NAF: []string{"96.02B"}, Synonyms: []string{"esthetique"}},
	{Key: "florist", Label: "fleuriste", GoogleType: "florist", OSMTags: []string{"shop=florist"}, NAF: []string{"47.76Z"}},
	{Key: "pharmacy", Label: "pharmacie", GoogleType: "pharmacy", OSMTags: []string{"amenity=pharmacy"}, NAF: []string{"47.73Z"}},
	{Key: "dentist", Label: "dentiste", GoogleType: "dentist", OSMTags: []string{"amenity=dentist", "healthcare=dentist"}, NAF: []string{"86.23Z"}},
	{Key: "doctor", Label: "médecin généraliste", GoogleType: "doctor", OSMTags: []string{"amenity=doctors"}, NAF: []string{"86.21Z"}},
	{Key: "physiotherapist", Label: "kinésithérapeute", GoogleType: "physiotherapist", OSMTags: []string{"healthcare=physiotherapist"}, NAF: []string{"86.90E"}},
	{Key: "veterinary", Label: "vétérinaire", GoogleType: "veterinary_care", OSMTags: []string{"amenity=veterinary"}, NAF: []string{"75.00Z"}},
	{Key: "lawyer", Label: "avocat", GoogleType: "lawyer", OSMTags: []string{"office=lawyer"}, NAF: []string{"69.10Z"}},
	{Key: "accountant", Label: "expert-comptable", GoogleType: "accounting", OSMTags: []string{"office=accountant"}, NAF: []string{"69.20Z"}, Synonyms: []string{"comptable"}},
	{Key: "real_estate", Label: "agence immobilière", GoogleType: "real_estate_agency", OSMTags: []string{"office=estate_agent"}, NAF: []string{"68.31Z"}, Synonyms: []string{"immobilier"}},
	{Key: "insurance", Label: "assurance", GoogleType: "insurance_agency", OSMTags: []string{"office=insurance"}, NAF: []string{"66.22Z"}},
	{Key: "car_repair", Label: "garage automobile", GoogleType: "car_repair", OSMTags: []string{"shop=car_repair"}, NAF: []string{"45.20A"}, Synonyms: []string{"garagiste", "mecanique auto"}},
	{Key: "car_dealer", Label: "concessionnaire automobile", GoogleType: "car_dealer", OSMTags: []string{"shop=car"}, NAF: []string{"45.11Z"}},
	{Key: "gym", Label: "salle de sport", GoogleType: "gym", OSMTags: []string{"leisure=fitness_centre"}, NAF: []string{"93.13Z"}, Synonyms: []string{"fitness"}},
	{Key: "hotel", Label: "hôtel", GoogleType: "hotel", OSMTags: []string{"tourism=hotel"}, NAF: []string{"55.10Z"}},
	{Key: "optician", Label: "opticien", GoogleType: "store", OSMTags: []string{"shop=optician"}, NAF: []string{"47.78A"}},
	{Key: "clothing", Label: "magasin de vêtements", GoogleType: "clothing_store", OSMTags: []string{"shop=clothes"}, NAF: []string{"47.71Z"}},
	{Key: "supermarket", Label: "supermarché", GoogleType: "supermarket", OSMTags: []string{"shop=supermarket"}, NAF: []string{"47.11D", "47.11F"}},
	{Key: "architect", Label: "architecte", GoogleType: "", OSMTags: []string{"office=architect"}, NAF: []string{"71.11Z"}},
	{Key: "it_services", Label: "société informatique", GoogleType: "", OSMTags: []string{"office=it"}, NAF: []string{"62.01Z", "62.02A"}, Synonyms: []string{"informatique", "esn"}},
	{Key: "marketing_agency", Label: "agence marketing", GoogleType: "", OSMTags: []string{"office=advertising_agency"}, NAF: []string{"73.11Z"}, Synonyms: []string{"agence web", "agence de communication"}},
	{Key: "cleaning", Label: "entreprise de nettoyage", GoogleType: "", OSMTags: []string{"craft=cleaning"}, NAF: []string{"81.21Z", "81.22Z"}},
	{Key: "driving_school", Label: "auto-école", GoogleType: "driving_school", OSMTags: []string{"amenity=driving_school"}, NAF: []string{"85.53Z"}},
	{Key: "funeral", Label: "pompes funèbres", GoogleType: "funeral_home", OSMTags: []string{"shop=funeral_directors"}, NAF: []string{"96.03Z"}},
}

var byKey = func() map[string]*Sector {
	m := make(map[string]*Sector, len(sectors))
	for i := range sectors {
		m[sectors[i].Key] = &sectors[i]
	}
	return m
}()

// Get returns the sector for key, or nil.
func Get(key string) *Sector { return byKey[strings.ToLower(key)] }

// All returns every sector sorted by key.
func All() []Sector {
	out := make([]Sector, len(sectors))
	copy(out, sectors)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Guess maps a free-text query to a sector when it clearly names one.
// fold must lowercase and strip accents (normalize.Fold).
func Guess(query string, fold func(string) string) *Sector {
	q := fold(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	for i := range sectors {
		s := &sectors[i]
		if q == s.Key || q == fold(s.Label) {
			return s
		}
		for _, syn := range s.Synonyms {
			if q == fold(syn) {
				return s
			}
		}
	}
	// singular/plural tolerance: "plombiers" -> "plombier"
	if strings.HasSuffix(q, "s") {
		return Guess(strings.TrimSuffix(q, "s"), fold)
	}
	return nil
}
