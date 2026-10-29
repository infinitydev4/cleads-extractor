# ADR-001 — Architecture du CLEADS Business Extractor

- **Statut** : accepté — 29/10/2026
- **Décision** : moteur Go indépendant (approche B) qui réutilise de façon ciblée deux éléments de `gosom/google-maps-scraper` (approche A) : la connaissance du format de données (réimplémentée) et le binaire upstream comme sidecar isolé pour le rendu navigateur.

## 1. Analyse de `gosom/google-maps-scraper` (commit `903b96d`, 21/10/2026)

| Aspect | Constat |
|---|---|
| **Architecture Go** | Monolithe (~25 000 lignes) : CLI, UI web, API REST, édition SaaS, provisionneurs DigitalOcean/Hetzner, runner AWS Lambda. `go.mod` tire ~250 modules (SDK AWS, SDK cloud, River, Playwright, et même `golangci-lint` en dépendance directe). |
| **Concurrence** | Bibliothèque `scrapemate` du même auteur : file de jobs priorisés, `-c` workers. Chaque job produit des jobs enfants (recherche → fiche → email). Un job en mode navigateur occupe une page Playwright pendant toute sa durée. |
| **Navigateur** | Pool Playwright : `-browser-pool-size` × `-pages-per-browser`, réutilisation de page limitée à 2, de navigateur à 200, images désactivées, option stealth Firefox. Le coût d'une instance Chromium se chiffre en centaines de Mo et domine la RAM. |
| **Récupération des données** | (1) Page de recherche : défilement du flux `div[role=feed]` par `eval` JS (profondeur `-depth`), collecte des liens. (2) Fiche : lecture de `window.APP_INITIALIZATION_STATE` puis parsing de tableaux JSON **positionnels** (`darray[11]` = nom, `[178][0][0]` = téléphone…). (3) « Fast mode » : `GET /search?tbm=map&pb=…` en HTTP pur, ~20 résultats par requête, sans navigateur. |
| **Options de performance** | `-c`, `-depth`, `-fast-mode`, taille du pool, pages par navigateur, proxies, `-exit-on-inactivity`. |
| **Gestion des jobs** | Runner fichier, runner web (SQLite + polling), runner base (`-produce` + PostgreSQL), River dans l'édition SaaS. En mode web, les résultats sont récupérés **en fin de job** sous forme de CSV. |
| **Déduplication** | Map FNV-64 en mémoire sur l'URL de fiche, par processus : pas de dédup inter-jobs, inter-sources ni inter-processus. |
| **Intégration** | API REST `/api/v1/jobs` (création, statut, téléchargement CSV), plugins d'écriture, export LeadsDB. |
| **Benchmarks** | Le README annonce « ~120 places/minute » ; aucun harnais de benchmark reproductible dans le dépôt. |
| **Fragilité** | Les index positionnels bougent (ex. statut passé de `[34][4][4]` à `[88][0]` vers juillet 2026, horaires déplacés en `[203]` en novembre 2025) : toute implémentation basée sur ce format demande une maintenance continue. |

## 2. Comparaison A / B

| Critère | A — Réutiliser / adapter le projet | B — Moteur indépendant | Retenu |
|---|---|---|---|
| **Vitesse** | ~120 fiches/min annoncées en mode navigateur (non vérifiable ici) ; le fast mode est rapide mais sans pagination ni dédup. | HTTP pur + parsing paresseux gjson (5,7× plus rapide que le décodage `[]any`, voir BENCHMARKS) ; plafond moteur mesuré à ~2 400 fiches uniques/s. | B |
| **Stabilité** | Dépend du rendu Chromium (timeouts, crashs, captchas) ; pas de reprise fine des tâches. | File PostgreSQL à bail + fencing, retries bornés, disjoncteur par source, reprise après crash vérifiée par tests. | B |
| **Mémoire** | Centaines de Mo par navigateur ; incompatible avec une API légère. | 31–65 Mo RSS mesurés pour un worker ; 51 Mo pour l'API sous charge. | B |
| **Maintenabilité** | Fork d'un monolithe de 250 dépendances qui évolue vite ; packages internes couplés à `scrapemate`/`exiter`. | 5 dépendances directes, sources découplées derrière une interface `Source`. | B |
| **Couverture de données** | Excellente (fiche complète, avis, emails). | Dépend des sources ; le mode navigateur reste utile pour certains champs. | A (en sidecar) |
| **Conformité** | 100 % scraping Google. | Sources officielles et ouvertes par défaut ; scraping en opt-in. | B |
| **Résultats progressifs** | CSV en fin de job. | Curseur stable + SSE pendant l'exécution. | B |

**Pourquoi ne pas importer le code upstream comme bibliothèque** : le package `gmaps` est couplé à `scrapemate` (modèle de job, navigateur) et à `exiter`, le module embarque ~250 dépendances, et son API interne n'est pas stable. Le coût de maintenance d'un fork dépasse celui d'une réimplémentation ciblée.

**Mise à jour (29/10/2026, après tests réels)** : le scraping HTTP est désormais la **source Google par défaut, sans clé API**. Il se fait en deux temps : la recherche paginée (`!8i` = offset vérifié en réel : 7 pages, 140 fiches distinctes) rend les fiches visibles immédiatement ; puis une tâche « détail » par entreprise appelle `/maps/preview/place` (l'endpoint JSON de l'application web, construit à partir du seul `data_id`) pour la fiche complète, sans page HTML ni navigateur. Les deux étapes partagent le même gouverneur de débit, puisque c'est la même IP qui interroge Google.

**Ce qui est réutilisé** :
1. *La connaissance du format* (chemins positionnels, paramètre `pb`, cookie de consentement, dérivation du CID depuis le `data_id`) réimplémentée dans `internal/source/gmapshttp` avec attribution MIT. Les fixtures upstream servent de tests de non-régression.
2. *Le binaire upstream inchangé* comme service Railway séparé (`gosom/google-maps-scraper -web`), piloté par `internal/source/gmapsbrowser` via son API REST. Chromium n'entre jamais dans le processus API, et on bénéficie des correctifs upstream sans fork.

## 3. Architecture retenue

```
                    ┌──────────────── Railway ────────────────────────────────────┐
  CRM / clients ──► │  api (ROLE=api) ×N        PostgreSQL           worker (ROLE=worker) ×M
   REST + SSE       │  validation, auth,  ◄──►  jobs, tasks (queue)  ◄──►  pools par source
                    │  lecture/export             businesses, keys        gouverneur + disjoncteur
                    │                              job_businesses          planificateur, enrichisseur
                    │                                                      │
                    │                         gmaps-scraper (optionnel) ◄──┘ (réseau privé)
                    └──────────────────────────────────────────────────────────────┘
                              Sources : Google Places API · OSM Overpass · SIRENE · (Maps HTTP / navigateur, opt-in)
```

### Cycle de vie d'un job
1. `POST /v1/jobs` : validation, création du job + une tâche `plan` (une transaction).
2. **Plan** : géocodage (Nominatim ou Google), choix des sources compatibles, quadrillage de la zone par source (taille de cellule propre à chaque source, plafond global de cellules).
3. **Search** : chaque cellule/page = une tâche. Résultat saturé (plafond de la source atteint) → la cellule est **divisée en 4** (quadtree adaptatif) jusqu'à la taille minimale de la source. Page suivante → nouvelle tâche.
4. **Persist** : normalisation (E.164, URL canonique, emails, CP), clés de blocage, filtre cache in-process, upsert en lot.
5. **Enrich** (option) : crawl du site (accueil + pages contact/mentions légales, robots.txt respecté, 1 requête simultanée par hôte, SSRF bloqué) → emails, téléphone, réseaux, SIRET.
6. **Finalisation** : job terminé quand plus aucune tâche n'est en attente/en cours ; événement émis une seule fois (webhook signé HMAC), quel que soit le motif de fin (normal, quota, annulation, échec).

### Modèle de concurrence
- Un **pool borné par source** (concurrence, débit et disjoncteur propres) : une source lente ou bloquée n'affame jamais les autres.
- Les pools louent des tâches **par lots dimensionnés à leurs slots libres** (`FOR UPDATE SKIP LOCKED`) : aucun verrou applicatif entre workers, scalabilité horizontale en ajoutant des réplicas.
- **Bail + heartbeat** : une tâche dont le worker meurt est reprise à l'expiration du bail ; le **fencing** par numéro de tentative rejette la complétion tardive de l'ancien worker (pas de double comptage).
- **Gouverneur AIMD** : débit divisé par 2 sur 429, remonté de +5 % par succès ; **disjoncteur** sur blocage (403/captcha) avec refroidissement exponentiel (30 s → 30 min).
- **Arrêt gracieux** : SIGTERM → plus de location, tâches en cours terminées (fenêtre configurable), sinon rendues immédiatement à la file.

### Déduplication (résolution d'entités)
Chaque fiche porte des **clés de blocage** de la plus forte à la plus faible : `pid:` (place_id), `cid:`, `siret:`, `osm:`, `np:` (nom normalisé + téléphone E.164), `ng:` (nom + geohash6 ≈ 1,2 × 0,6 km), `na:` (nom + CP), `nd:` (nom + domaine). Deux fiches partageant une clé sont la même entreprise et sont **fusionnées** (champs vides complétés, listes unies, sources cumulées).
- En base : verrous consultatifs transactionnels sur les clés, pris dans un ordre global (pas d'interblocage) → deux workers ne créent jamais la même entreprise deux fois (testé avec 8 écrivains concurrents).
- En mémoire : cache borné, sharded, sans allocation, qui écarte avant la base les retours répétés d'une même source sur des cellules qui se chevauchent.
- `max_results` est un **quota atomique** (testé à 8 écrivains : exactement 75/75).

### Choix techniques
| Sujet | Choix | Raison |
|---|---|---|
| File de tâches | PostgreSQL `SKIP LOCKED` | Un seul service managé sur Railway, transactions communes avec les données, mesuré à ~500 tâches/s sur un PG local. |
| Écritures | `COPY` (nouvelles fiches) + batch pipeliné (mises à jour) + `unnest` (clés, liens) | Un aller-retour par étape, quelle que soit la taille du lot. |
| Parsing Maps | gjson, lecture en un seul passage des index utiles | 22 allocations au lieu de 29 885 par fiche. |
| HTTP | `net/http` standard (routage Go 1.22+), pas de framework | Moins de dépendances, latence API ≈ 1 ms. |
| Export | Streaming par pages de 2 000 en keyset | Mémoire constante, export possible pendant que le job tourne. |
| Mémoire | `GOMEMLIMIT` dérivé du cgroup (90 %) | Le GC travaille avant l'OOM killer de Railway. |
| Image | distroless static, non-root | ~20 Mo, surface d'attaque minimale. |
