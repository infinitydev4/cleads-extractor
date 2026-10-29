# CLEADS Business Extractor API

API REST en Go pour **rechercher, extraire, normaliser, dédupliquer, enrichir et exporter** des fiches d'entreprises par secteur d'activité et zone géographique, avec des résultats disponibles **pendant** l'extraction.

- **Sources** : **scraping Google Maps par défaut, sans clé API** (recherche + fiche complète en HTTP, sans navigateur : catégories, horaires, téléphone, site, note, avis, photos, attributs, propriétaire, affluence), OpenStreetMap, SIRENE (registre public français), et en option Google Places API ou un sidecar navigateur [`gosom/google-maps-scraper`](https://github.com/gosom/google-maps-scraper).
- **Moteur** : file PostgreSQL (`SKIP LOCKED`) partagée par N workers, pools par source avec débit adaptatif et disjoncteur, quadrillage adaptatif (quadtree) des zones, déduplication inter-sources par résolution d'entités.
- **Mesuré en réel sur Google** : 350 fiches complètes/min à la cadence par défaut (8 req/s), jusqu'à ~1 000/min à 20 req/s, 0 blocage, < 100 Mo de RAM ; API à ~25 000 req/s (p99 3 ms). Voir [docs/BENCHMARKS.md](docs/BENCHMARKS.md) : rien n'est promis sans mesure.

| Document | Contenu |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Analyse de gosom/google-maps-scraper, comparaison A/B, architecture retenue |
| [docs/BENCHMARKS.md](docs/BENCHMARKS.md) | Résultats mesurés, méthode, projection de capacité |
| [docs/RAILWAY.md](docs/RAILWAY.md) | Déploiement pas à pas, dimensionnement, exploitation |
| [docs/COMPLIANCE.md](docs/COMPLIANCE.md) | Licences des sources, RGPD/CNIL, sécurité |
| `GET /docs` | Référence OpenAPI interactive (spécification : `internal/api/openapi.yaml`) |

## Démarrage rapide

```bash
# sans base de données, avec la source synthétique (aucun appel externe)
make run
curl -s localhost:8080/v1/jobs -d '{"query":"plombier","center":{"lat":45.76,"lng":4.84},"radius_m":3000}'
```

Avec PostgreSQL et les sources réelles :

```bash
cp .env.example .env   # renseigner API_KEYS, GOOGLE_PLACES_API_KEY…
make up                # docker compose : postgres + api + 2 workers
```

## Utilisation

```bash
KEY=...   # la clé elle-même, c.-à-d. la partie après « admin: » dans API_KEYS

# 1. lancer un job (scraping Google Maps + OSM + SIRENE par défaut)
curl -s -X POST "$URL/v1/jobs" -H "Authorization: Bearer $KEY" -H "Idempotency-Key: crm-123" \
  -d '{"query":"plombier","location":"Lyon, France","enrich":true,"max_results":2000}'

# 2. suivre la progression (Server-Sent Events)
curl -N "$URL/v1/jobs/$ID/events" -H "Authorization: Bearer $KEY"

# 3. lire les résultats au fil de l'eau (curseur stable : ne renvoie que les nouvelles fiches)
curl -s "$URL/v1/jobs/$ID/businesses?cursor=0&limit=500&useful=true" -H "Authorization: Bearer $KEY"

# 4. exporter (streaming, possible pendant l'exécution)
curl -s "$URL/v1/jobs/$ID/export?format=csv-excel&has_phone=true" -H "Authorization: Bearer $KEY" -o leads.csv

# base de prospection consolidée (tous jobs, dédupliquée)
curl -s "$URL/v1/businesses?postal_code=69&category=plomb&has_email=true" -H "Authorization: Bearer $KEY"
```

**International** : n'importe quel pays couvert par Google Maps, par exemple `{"query":"restaurant","location":"Dubai Marina","language":"en"}` (testé en réel). Sans `country`, le pays est déduit du lieu géocodé (recherche mondiale) ; `language` choisit la langue des catégories et des avis (`en`, `ar`, `fr`…). Les téléphones sont rendus au format international E.164 quel que soit le pays. SIRENE n'est utilisé que pour la France.

Par défaut, les commerces sans rapport que Google ajoute en fin de liste sont écartés (`"include_unrelated": true` pour tout garder). Un job accepte une zone sous l'une des trois formes : `location` (géocodée), `bbox`, ou `center` + `radius_m`. `sector` (voir `GET /v1/sectors`) traduit l'activité dans la classification native de chaque source (type Google, tag OSM, code NAF) ; `query` permet une recherche libre. `GET /v1/sources` liste les sources actives sur le serveur.

### Les quatre compteurs
Chaque job expose `raw_found` (tout ce que les sources ont renvoyé), `unique` (après dédup), `complete` (identité, adresse, téléphone et site ou email) et `crm_useful` (en activité et joignable par au moins un canal). Les mêmes séries existent dans `/metrics`, par source.

## Structure

```
cmd/server        binaire (ROLE=api|worker|all)
cmd/bench         benchmark bout-en-bout reproductible
internal/api      handlers REST, SSE, export, auth, OpenAPI
internal/engine   planification, pools par source, retries, quadtree, webhooks
internal/source   placesapi · osm · sirene · gmapshttp · gmapsbrowser · mock
internal/store    interface + pgstore (production) + memstore (tests/dev)
internal/dedup    clés de blocage, cache de dédup borné
internal/normalize téléphones E.164, URLs, emails, noms, SIRET
internal/enrich   crawler de sites (emails, réseaux, SIRET), robots.txt, anti-SSRF
internal/geo      grille, quadtree, geohash, distances
migrations        schéma SQL embarqué
```

## Tests

```bash
make test        # vet + tests unitaires et bout-en-bout (memstore)
make race        # détecteur de concurrence
make test-pg TEST_DSN=postgres://...   # intégration PostgreSQL (tables vidées !)
make bench       # micro-benchmarks
```

La suite couvre : parsing sur fixtures réelles Google Maps, chaque source contre un serveur simulé (pagination, saturation, erreurs 429/403/5xx, bascule de miroirs), normalisation, dédup inter-sources, 8 écrivains concurrents sur PostgreSQL (aucun doublon créé), quota `max_results` exact sous concurrence, exclusivité des baux et fencing, reprise après bail expiré, annulation, API (auth, isolation des clés, idempotence, SSE, pagination, export, rate-limit).

## Configuration
Toutes les variables sont documentées dans [.env.example](.env.example).

## Licence et attribution
Le parsing Google Maps s'appuie sur le travail de gosom/google-maps-scraper (MIT, © 2023 Georgios Komninos). Les exports issus d'OpenStreetMap doivent porter la mention « © les contributeurs d'OpenStreetMap » (ODbL).
