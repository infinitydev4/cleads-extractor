# Benchmarks — résultats mesurés

> Tous les chiffres ci-dessous ont été **mesurés** le 29/10/2026, y compris le scraping Google Maps réel (§3). Rien n'a encore été mesuré sur Railway : les projections de la section 5 sont des calculs, à confirmer par les mêmes commandes une fois déployé.

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

### 2.1 Coût du tee de journaux (29/10/2026, ajout de l'observabilité)

Même machine. PostgreSQL 16.14 jetable (`shared_buffers=256MB`). Même scénario qu'au §2 : 20 × 20 km, concurrence 64, 2 % d'échecs transitoires, soit ~100 avertissements par run. 3 runs alternés par configuration : binaire d'avant (`bench-before`, journaux jetés) et binaire d'après avec `-log-tee` (logger de production JSON + copie des WARN+ vers `events` + tampon circulaire + battements de cœur des workers). Chaque run produit 23 529 uniques.

| Store | Workers | Uniques/s avant (médiane ; min–max) | Uniques/s après (médiane ; min–max) | RSS avant → après | Événements stockés / perdus |
|---|---|---|---|---|---|
| PostgreSQL | 1 | 1 345 (1 225–1 410) | 1 338 (1 308–1 615) | 32,0 → 32,8 Mo | ~97 / 0 |
| PostgreSQL | 4 | 2 201 (1 997–2 888) | 2 137 (1 846–3 090) | 61,4 → 63,4 Mo | ~106 / 0 |
| mémoire | 1 | 1 309 (1 266–1 628) | 1 606 (1 596–1 618) | 80,1 → 81,3 Mo | ~100 / 0 |

**Lecture** : les écarts restent sous le bruit d'un run à l'autre (±20 % sur PostgreSQL local). Aucune régression n'est mesurable. Les micro-benchmarks (`go test -bench Tee ./internal/obs`) donnent le coût unitaire : un avertissement copié coûte 2,5 µs dans l'appelant ; une ligne INFO coûte ~1,9 µs au lieu de 1,2 µs pour un `JSONHandler` nu (tampon circulaire et masquage). Avec une base bloquée et une file de 16, 40 000 avertissements sur 8 goroutines prennent 0,16 s et sont comptés comme perdus : le moteur n'attend jamais.

Non re-mesuré : le débit de l'API sous `ab` (§5). Le surcoût attendu est de ~1 µs par requête (une ligne INFO), à comparer à un p50 de 2 ms.

## 3. Scraping Google Maps réel — sans clé API (source par défaut)

Source `google_maps_http` : recherche (20 fiches/page, paginée) puis fiche détaillée de chaque entreprise (`/maps/preview/place`, ~40 Ko), sans navigateur. Une seule IP résidentielle, aucun proxy, base vide à chaque run, enrichissement des sites désactivé sauf mention.

### 3.1 Montée en cadence

| Cadence visée | Requête / zone | Durée | Req. recherche | Req. détail | Erreurs | Uniques (fiche complète) | Utiles CRM | **Fiches/min** | Latence moy. recherche / détail | RSS |
|---|---|---|---|---|---|---|---|---|---|---|
| 2 req/s | restaurant, Lyon 2e | 483 s | 141 | 820 | 0 | 820 | 778 | 102 | 0,67 / 0,46 s | 34 Mo |
| 5 req/s | restaurant, Lyon 3e | 252 s | 169 | 1 079 | 0 | 1 079 | 1 003 | 257 | 0,67 / 0,44 s | 34 Mo |
| 10 req/s | restaurant, Lyon 7e | 115 s | 141 | 972 | 0 | 972 | 913 | 505 | 0,66 / 0,40 s | 36 Mo |
| 20 req/s (16,4 effectives) | restaurant, Lyon 6e | 35 s | 83 | 493 | 0 | 493 | 476 | 840 | 0,67 / 0,40 s | 36 Mo |
| 20 req/s soutenus (19,5) | coiffeur, Lyon entier | 83 s | 213 | 1 406 | 0 | 1 406 | 1 324 | 1 017 | 0,56 / 0,37 s | 38 Mo |

**Lecture** : 0 erreur et 0 blocage jusqu'à ~20 req/s sur des sessions de quelques minutes ; le débit croît linéairement avec la cadence (la latence Google ne bouge pas). Une fiche complète coûte en moyenne **1,2 à 1,3 requête** (recherche amortie + détail).

### 3.2 Exhaustivité selon la maille (coiffeurs, Lyon entier, 20 req/s)

| Maille | Req. recherche | Uniques | Remarque |
|---|---|---|---|
| 1,5 km | 213 | 1 406 | référence |
| **0,75 km (défaut retenu)** | 376 | **1 658 (+18 %)** | meilleur compromis |
| 0,5 km | 557 | 1 494 | 108 erreurs transitoires, latence ×3 : Google ralentit après ~25 000 requêtes cumulées en ~15 min à 20 req/s |

→ Défauts retenus : **maille 0,75 km, 8 req/s et 24 requêtes simultanées par IP**, soit une marge confortable sous le seuil où Google commence à ralentir.

### 3.3 Validation finale avec les valeurs par défaut (boulangeries, Lyon entier, enrichissement des sites activé)

| Run | Durée | Requêtes | Erreurs | Uniques | Utiles CRM | Fiches/min | Sites crawlés |
|---|---|---|---|---|---|---|---|
| avant filtre de pertinence | 189 s | 1 495 | 1 (500 rejoué) | 1 197 | 1 043 | 380 | 569 |
| **après filtre de pertinence** | **154 s** | 1 190 | **0** | **891** | 771 | 348 | 383 |

Le filtre écarte les commerces sans rapport que Google ajoute en fin de liste (27 % des résultats : boucheries, supermarchés, tabacs…) et économise d'autant les requêtes de détail. Catégories restantes : Boulangerie 646, Pâtisserie 131, Chocolatier 26, Traiteur 17…

Couverture des 891 fiches :

| Champ | Fiches | Part |
|---|---|---|
| Fiche détaillée (catégories complètes, horaires, propriétaire, photos) | 891 | 100 % |
| Avis (texte, auteur, date, réponse du propriétaire) | 842 | 95 % |
| Téléphone (E.164) | 755 | 85 % |
| Site web | 451 | 51 % |
| Email (site crawlé) | 263 | 30 % |
| Description | 112 | 13 % |
| Statut fermé correctement détecté | 6/6 sur un échantillon contrôlé | — |

### 3.4 International : Dubaï (Émirats arabes unis)

Jobs lancés depuis la même IP française, **sans préciser le pays** (détecté par géocodage : AE).

| Job | Durée | Req. recherche / détail | Erreurs | Uniques | Téléphones `+971` | Sites | Avis | Fiches/min |
|---|---|---|---|---|---|---|---|---|
| `plumber`, Dubai Marina, `enrich` | 30 s | 63 / 34 | 0 | 34 | 33 / 34 (100 % des numéros) | 26 (17 emails trouvés) | 29 | — |
| `restaurant`, Dubai Marina | 130 s | 140 / 862 | 0 | 862 | 769 | 551 | 828 | ~400 |

Recherche en arabe (`سباك`) également validée. Particularités traitées : numéros pris dans le format international fourni par Google (valable pour tous les pays), pays et ville lus dans le bloc adresse (pas de code postal aux Émirats), et géocodage mondial quand le pays n'est pas fourni. Sans ce dernier point, « Dubai Marina » se résolvait en un lieu homonyme… à Rennes.

### 3.5 Depuis l'IP d'un VPS OVH (production, 29/10/2026)

Mêmes tests, lancés par l'API publique du serveur de production : **OVHcloud VPS-1** (2 vCPU, 4 Go, Gravelines), donc une **IP de datacenter**, sans proxy. Chaque job interroge uniquement Google Maps.

| Cadence | Job | Durée | Requêtes | Erreurs | Fiches uniques | Fiches/min |
|---|---|---|---|---|---|---|
| 4 req/s | boulangeries, Lyon 3e | 85 s | 311 | 0 | 279 | 197 |
| 4 req/s | boulangeries, Lyon 6e | 36 s | 137 | 0 | 180 | 297 |
| 8 req/s | boulangeries, Lyon 7e | 32 s | 216 | 5 (HTTP 500, rejouées avec succès) | 273 | 513 |
| 12 req/s | restaurants, Lyon 8e | 29 s | 302 | 0 | 262 | 550 |
| 8 req/s | coiffeurs, Lyon entier + enrichissement | 275 s | 1 499 | 0 | 1 137 | 249 |
| 8 req/s | plombiers, Lyon entier + enrichissement | 85 s | 569 | 0 | 359 | 254 |

**≈ 3 000 requêtes en 15 minutes, aucun blocage, aucune limitation de débit (429), aucune tâche perdue.** Latence moyenne : 0,4 à 0,5 s en recherche, 0,35 à 0,47 s en détail. Consommation du serveur : worker 38 à 63 Mo de RAM, PostgreSQL 86 à 122 Mo, hôte à 830 Mo sur 3,8 Go ; le CPU du worker atteint 130 à 185 % de 2 cœurs pendant le crawl des sites (voir `docs/DEPLOY_VPS.md` §5).

**Limites de cette mesure** : environ 5 minutes par job, une seule adresse, quelques milliers de requêtes ; la tenue sur 24 h et sur plusieurs jours n'est **pas** mesurée. Cadence de production retenue : **8 req/s**.

### 3.6 Ce que cela implique, et ce qui n'est pas mesuré

- **Une IP à 8 req/s ≈ 350 fiches complètes/min.** 50 000 fiches/jour ≈ 65 000 requêtes, soit **0,8 req/s en moyenne** ; 100 000/jour ≈ 1,6 req/s. Les deux objectifs sont donc atteignables **en quelques heures** de travail effectif par jour, sur une IP.
- **Non mesuré** : la tolérance de Google sur 24 h en continu, et depuis les IP de datacenter de Railway (en général plus surveillées que les IP résidentielles). Avant d'y monter en volume, lancer le même protocole depuis Railway ; prévoir des proxies résidentiels (`GMAPS_PROXIES`) pour 100 000+/jour.
- Un panic isolé (1 tâche sur 2 051) est survenu pendant le run à 0,5 km ; la tâche a été rejouée sans perte. Les parseurs convertissent désormais toute anomalie en erreur, sauvegardent la réponse fautive (`GMAPS_DEBUG_DIR`) et ont été fuzzés 60 s sans défaut ; la cause exacte n'a pas été reproduite.

## 4. Tests réels des sources ouvertes (OSM, SIRENE)

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

## 5. API (`ab`, 50 connexions keep-alive, PostgreSQL local)

| Endpoint | Débit | p50 | p99 |
|---|---|---|---|
| `GET /healthz` | 52 063 req/s | 2 ms | 3 ms |
| `GET /v1/jobs/{id}` (lecture PG) | **24 867 req/s** | 2 ms | 3 ms |
| `GET /v1/jobs/{id}/businesses?limit=100` | 3 655 req/s | 14 ms | 18 ms |

RSS du processus API après charge : **51 Mo**. Processus `ROLE=all` après les jobs réels (enrichissement compris) : **62 Mo**. Le rate-limit par clé (20 req/s par défaut) a correctement renvoyé des 429 au-delà.

## 6. Capacité sur Railway — calcul, **non mesuré**

Le moteur ne sera pas le facteur limitant. Le débit utile quotidien se calcule ainsi :

```
utiles/jour = requêtes/jour × résultats/requête × (1 − taux de doublons) × part utile
```

| Source | Contrainte à mesurer en production | Ordre de grandeur |
|---|---|---|
| Google Places (Text Search) | quota/minute du projet GCP et coût par requête (voir la grille tarifaire Google en vigueur) | 20 résultats/requête ; 50 000 uniques/jour à 50 % de doublons ≈ 5 000 requêtes/jour |
| OSM Overpass public | usage raisonnable, 1–2 requêtes simultanées | centaines de fiches par requête ; instance privée recommandée au-delà de quelques milliers de requêtes/jour |
| SIRENE (API Recherche d'entreprises) | 7 req/s par IP | 25 établissements/page ; pas de contacts |
| **Google Maps HTTP (défaut)** | mesuré sans erreur jusqu'à ~20 req/s depuis une IP résidentielle ; à re-mesurer depuis les IP Railway | ~1,25 requête par fiche complète (§3) |
| Google Maps navigateur (sidecar) | Chromium, plusieurs centaines de Mo | non mesuré (Docker indisponible ici) |

**Objectif initial (10 000–50 000/jour)** : atteint en mesure réelle par le scraping HTTP sur une seule IP (350 fiches complètes/min à la cadence par défaut). **Objectif avancé (100 000+/jour)** : 1,6 req/s en moyenne suffisent ; le facteur de risque est la tolérance de Google sur la durée et depuis les IP Railway, à mesurer avant d'y compter (proxies résidentiels recommandés). Le moteur lui-même a une marge ×40.

## Reproduire

```bash
make bench                                     # micro-benchmarks
make test-pg TEST_DSN=postgres://…             # tests d'intégration
go run ./cmd/bench -store pg -dsn "$DATABASE_URL" -workers 4 -area-km 20 -concurrency 64
ab -n 20000 -c 50 -k -H "Authorization: Bearer $KEY" "$URL/v1/jobs/$JOB_ID"
```
