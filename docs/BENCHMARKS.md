# Benchmarks — résultats mesurés

> Tous les chiffres ci-dessous ont été **mesurés** le 29/10/2026. Rien n'a encore été mesuré sur Railway : les projections de la section 5 sont des calculs, à confirmer par les mêmes commandes une fois déployé.

**Environnement** : MacBook (Apple Silicon arm64, 10 cœurs), Go 1.27.3, PostgreSQL 16.14 local (`shared_buffers=256MB`, fsync activé), connexion Internet résidentielle pour les tests réels.

On mesure séparément quatre débits, car le débit brut seul masque les doublons et les fiches vides :

| Débit | Définition |
|---|---|
| **brut** (`raw_found`) | tout ce que les sources renvoient, doublons compris |
| **unique** (`unique`) | entreprises distinctes rattachées au job après dédup |
| **complet** (`complete`) | nom + adresse/ville + téléphone + (site ou email) |
| **utile CRM** (`crm_useful`) | non fermée définitivement + au moins un canal de contact |

## 1. Micro-benchmarks (`make bench`)

| Benchmark | Résultat | Allocations |
|---|---|---|
| Parsing d'une fiche Maps 170 Ko — **gjson (retenu)** | 225 µs, 794 Mo/s | 173 Ko, **22 allocs** |
| Parsing d'une fiche Maps — `json.Unmarshal` en `[]any` (approche courante) | 1 281 µs, 139 Mo/s | 1 045 Ko, **29 885 allocs** |
| Parsing d'une réponse de recherche (20 fiches) | 126 µs | 328 allocs |
| Clés de déduplication d'une fiche | 325 ns | 8 allocs |
| Cache de dédup, 10 goroutines en parallèle | 52 ns/op | 0 |
| Normalisation d'un nom (clé de comparaison) | 308 ns | 2 allocs |
| Normalisation téléphone → E.164 | 89 ns | 3 allocs |
| Écriture d'une ligne CSV | 335 ns | 0 |

→ Le parsing gjson est **5,7× plus rapide** et alloue **6× moins de mémoire** que le décodage générique.

## 2. Plafond du moteur bout-en-bout (`cmd/bench`)

Source synthétique (`mock`) : fiches sur une grille fixe (40/km²), cellules qui « débordent » de 10 % (comme les vrais moteurs), 3 pages de 20, 2 % d'échecs transitoires. Zone 20 × 20 km, cellules de 1 km, concurrence 64 par worker. Les « workers » sont des instances du moteur dans un même processus, partageant la même base (émulation de réplicas).

| Store | Workers | Latence source | Durée | Tâches/s | Brut/s | **Uniques/s** | Utiles CRM/s | RSS processus |
|---|---|---|---|---|---|---|---|---|
| mémoire | 1 | 150 ms | 17,5 s | 278 | 4 324 | 1 346 | 1 258 | 80 Mo |
| PostgreSQL | 1 | 150 ms | 17,7 s | 274 | 4 267 | 1 328 | 1 242 | 31 Mo |
| PostgreSQL | 2 | 150 ms | 14,3 s | 339 | 5 285 | 1 645 | 1 538 | 38 Mo |
| PostgreSQL | 4 | 150 ms | 8,8 s | 550 | 8 564 | **2 665** | 2 492 | 56 Mo |
| PostgreSQL | 4 | 1 ms | 9,7 s | 499 | 7 769 | 2 418 | 2 260 | 65 Mo |

Chaque run : 23 529 entreprises uniques, taux de doublons 68 % (chevauchements + raffinement quadtree).

**Lecture** : à 4 workers, supprimer la latence ne change plus rien → le goulot est **PostgreSQL** (≈ 500 tâches/s ≈ 2 400 uniques/s sur cette machine), pas le code Go. Ce plafond est **deux ordres de grandeur au-dessus** de l'objectif (50 000/jour ≈ 0,6/s en moyenne). La limite réelle sera celle des fournisseurs.

## 3. Tests réels (sources publiques, faible volume)

| Job | Sources | Brut | Uniques | Téléphone | Site | Email | Utiles CRM | Durée |
|---|---|---|---|---|---|---|---|---|
| `sector=plumber`, `Lyon` (agglomération) | OSM + SIRENE | 1 996 | 697 | 0 | 0 | 0 | 0 | 45 s |
| `sector=restaurant`, `Lyon 1er`, `enrich=true` | OSM | 241 | 237 | 146 (100 % en E.164) | 125 | 84* | **156 (66 %)** | 130 s |

\* mesuré avant le filtrage des emails gabarits et des emails d'agences web ajouté ensuite (le chiffre réel sera un peu plus bas, et plus propre).

Enseignements :
- **SIRENE** apporte l'existence légale et le SIRET (déduplication et qualification fiables) mais **aucun canal de contact** : seul, il ne produit pas de fiches utiles CRM. Il doit être combiné avec Places/OSM et l'enrichissement.
- Le serveur Overpass public a renvoyé `502 at capacity` pendant le premier test : la bascule sur des miroirs et le report long ont été ajoutés. Pour du volume, il faut une instance Overpass privée.
- L'enrichissement domine la durée (125 sites, ≤ 4 pages chacun, 1 requête simultanée par hôte) mais ne bloque pas les résultats : les fiches sont disponibles dès la recherche, puis complétées.
- Le filtre d'activité SIRENE s'applique à l'entreprise, pas à l'établissement : corrigé (filtre NAF par établissement, établissements fermés et `statut_diffusion = P` exclus).

## 4. API (`ab`, 50 connexions keep-alive, PostgreSQL local)

| Endpoint | Débit | p50 | p99 |
|---|---|---|---|
| `GET /healthz` | 52 063 req/s | 2 ms | 3 ms |
| `GET /v1/jobs/{id}` (lecture PG) | **24 867 req/s** | 2 ms | 3 ms |
| `GET /v1/jobs/{id}/businesses?limit=100` | 3 655 req/s | 14 ms | 18 ms |

RSS du processus API après charge : **51 Mo**. Processus `ROLE=all` après les jobs réels (enrichissement compris) : **62 Mo**. Le rate-limit par clé (20 req/s par défaut) a correctement renvoyé des 429 au-delà.

## 5. Capacité sur Railway — calcul, **non mesuré**

Le moteur ne sera pas le facteur limitant. Le débit utile quotidien se calcule ainsi :

```
utiles/jour = requêtes/jour × résultats/requête × (1 − taux de doublons) × part utile
```

| Source | Contrainte à mesurer en production | Ordre de grandeur |
|---|---|---|
| Google Places (Text Search) | quota/minute du projet GCP et coût par requête (voir la grille tarifaire Google en vigueur) | 20 résultats/requête ; 50 000 uniques/jour à 50 % de doublons ≈ 5 000 requêtes/jour |
| OSM Overpass public | usage raisonnable, 1–2 requêtes simultanées | centaines de fiches par requête ; instance privée recommandée au-delà de quelques milliers de requêtes/jour |
| SIRENE (API Recherche d'entreprises) | 7 req/s par IP | 25 établissements/page ; pas de contacts |
| Google Maps HTTP / navigateur | seuil de blocage inconnu, proxies nécessaires | **à mesurer, ne rien promettre** |

**Objectif initial (10 000–50 000/jour)** : atteignable avec un seul worker si les quotas Places ou une instance Overpass le permettent. **Objectif avancé (100 000+/jour)** : le moteur le supporte (plafond mesuré ×40), mais la faisabilité dépend uniquement des quotas fournisseurs, du budget Places et, pour le scraping, de la tolérance au blocage. À valider avec `cmd/bench` (plafond moteur sur l'infra Railway) puis avec un job réel instrumenté (`/metrics`).

## Reproduire

```bash
make bench                                     # micro-benchmarks
make test-pg TEST_DSN=postgres://…             # tests d'intégration
go run ./cmd/bench -store pg -dsn "$DATABASE_URL" -workers 4 -area-km 20 -concurrency 64
ab -n 20000 -c 50 -k -H "Authorization: Bearer $KEY" "$URL/v1/jobs/$JOB_ID"
```
