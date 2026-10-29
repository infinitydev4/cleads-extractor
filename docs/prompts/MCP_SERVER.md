# Prompt — Implémenter un serveur MCP pour CLEADS Business Extractor

> À donner tel quel à l'agent chargé de l'implémentation. Il travaille dans le dépôt `cleads-extractor/` (module Go `github.com/cleads/business-extractor`).

---

## Ta mission

Ajoute un serveur **MCP (Model Context Protocol)** à l'API CLEADS Business Extractor pour que des agents LLM (Claude Desktop, Claude Code, agents maison) puissent **tout faire avec l'API** :
- lancer des recherches et des extractions ;
- suivre leur progression ;
- lire, filtrer et exporter les résultats ;
- interroger la base de prospection consolidée ;
- consulter l'état du système : santé, métriques, file d'attente, **journaux et erreurs**, pour diagnostiquer seuls un job qui échoue.

Le travail est terminé quand :
1. un agent connecté peut enchaîner « cherche les plombiers de Lyon avec emails → attends la fin → donne-moi les 50 meilleures fiches → pourquoi 3 tâches ont échoué ? » sans autre outil ;
2. tout est testé, documenté et déployable comme aujourd'hui (Railway / VPS, même image).

Lis d'abord `README.md`, `docs/ARCHITECTURE.md`, `docs/BENCHMARKS.md`, `docs/COMPLIANCE.md` et `internal/api/openapi.yaml`.

---

## Ce qui existe déjà (ne le réécris pas)

**Stack** : Go 1.27, `net/http` standard (routage Go 1.22+ avec motifs `"GET /v1/jobs/{id}"`), `log/slog`, PostgreSQL via pgx v5 (ou `memstore` en dev et en tests), Prometheus. Cinq dépendances directes seulement : n'en ajoute qu'en le justifiant.

**Binaire** `cmd/server` avec `ROLE=api|worker|all`. L'API et les workers partagent une base PostgreSQL qui sert aussi de file de tâches (`FOR UPDATE SKIP LOCKED`). Il peut y avoir N workers sur des machines différentes.

**Paquets** (`internal/`) :

| Paquet | Rôle |
|---|---|
| `api` | Handlers REST, auth par clé API (SHA-256, temps constant), rate-limit par clé, isolation des jobs par clé (la clé `admin` voit tout), SSE, export streaming, OpenAPI embarquée (`openapi.yaml`). Point d'entrée : `api.New(cfg, store, engine, metrics, log, version).Handler()`. |
| `engine` | Planification (géocodage → quadrillage), pools par source, gouverneur de débit et disjoncteur, retries, quadtree, tâches `plan` / `search` / `details` / `enrich`, finalisation et webhooks. `engine.PlanTask(job)`, `eng.SelectSources(spec)`, `eng.Sources()`. |
| `source/*` | `gmapshttp` (**scraping Google Maps, source par défaut, sans clé**), `gmapsbrowser`, `placesapi`, `osm`, `sirene`, `mock` (synthétique, pour les tests). |
| `store` | Interface `store.Store` ; implémentations `pgstore` (prod) et `memstore` (dev, tests). |
| `domain` | `Job`, `JobSpec`, `Counters`, `Task`, `Business`, `Details` (avis, photos, attributs…). |
| `dedup`, `normalize`, `enrich`, `export`, `geo`, `geocode`, `taxonomy`, `metrics`, `config` | Voir les noms. |

**API REST** (toutes les routes `/v1/*` et `/metrics` exigent `Authorization: Bearer <clé>` ou `X-API-Key`) :

```
GET    /healthz  /readyz  /openapi.yaml  /docs          (publics)
GET    /v1/sources                     sources actives et profil (débit, cellule…)
GET    /v1/sectors                     taxonomie (clé, libellé, type Google, tags OSM, NAF)
POST   /v1/jobs                        créer un job (Idempotency-Key supporté) → 202
GET    /v1/jobs?status=&limit=&before= lister
GET    /v1/jobs/{id}                   statut + compteurs
DELETE /v1/jobs/{id}                   annuler
GET    /v1/jobs/{id}/businesses        résultats paginés par curseur (cursor, limit, filtres)
GET    /v1/jobs/{id}/events            SSE de progression
GET    /v1/jobs/{id}/export?format=csv|csv-excel|jsonl  export streaming
GET    /v1/businesses                  base consolidée (filtres, curseur)
GET    /v1/businesses/{id}
GET    /v1/stats                       nombre de fiches, jobs par statut, file par source
GET    /metrics                        Prometheus
```

Filtres des listes : `min_completeness`, `has_phone`, `has_email`, `has_website`, `useful`, `city`, `postal_code` (préfixe), `category`, `q` (nom).

**JobSpec** (`POST /v1/jobs`) :
- `query` ou `sector` (obligatoire) ;
- **une seule** zone parmi `location` (géocodée dans le monde entier), `bbox` et `center` + `radius_m` ;
- options : `country` (facultatif, déduit du lieu), `language` (`fr`, `en`, `ar`…), `sources`, `grid_cell_km`, `max_results` (quota atomique), `enrich`, `include_unrelated`, `webhook_url`, `priority`.

**Compteurs d'un job** : `tasks_total/done/failed`, `raw_found`, `unique`, `complete`, `crm_useful`, `enriched`. Un job passe par les statuts `queued` → `running` → `completed | failed | cancelled`. Il n'est terminé qu'après **toutes** ses tâches de détail et d'enrichissement, ce qui prend plusieurs minutes pour des milliers de fiches (≈ 350 fiches complètes/min à la cadence par défaut). Les résultats sont lisibles **pendant** l'exécution.

**Métriques** (Prometheus) : `cleads_records_{raw,unique,complete,crm_useful,unrelated}_total{source}`, `cleads_source_requests_total{source,outcome=ok|error|rate_limited|blocked|permanent}`, `cleads_source_request_seconds`, `cleads_source_rate_limit`, `cleads_source_circuit_open`, `cleads_tasks_in_flight`, `cleads_tasks_pending`, `cleads_store_upsert_seconds`, `cleads_enrich_total{outcome}`, `cleads_http_*`, `cleads_dedup_cache_hits_total`.

**Ce qui n'existe pas encore et dont tu as besoin** :
- **Aucun accès aux journaux par l'API.** `slog` écrit sur la sortie standard de chaque processus. Le processus API ne voit pas les journaux des workers, qui peuvent tourner sur d'autres machines.
- **Aucune route pour les tâches d'un job.** La table `tasks` conserve pourtant les tâches échouées (`status='failed'`, `last_error`, `attempts`, `source`, `payload`) ; les tâches réussies sont supprimées.

---

## Ce que tu dois construire

### 1. Observabilité accessible par l'API (prérequis de l'outil « logs »)

Les erreurs doivent être lisibles depuis n'importe quel réplica de l'API, quel que soit le worker qui les a produites. Approche attendue :
- **Table d'événements en base**, via une nouvelle migration `002_*.sql`. Ne modifie pas `001_init.sql` : elle est déjà déployée.
  - Colonnes : `ts`, `level`, `msg`, `job_id`, `task_id`, `source`, `worker`, `attrs jsonb`.
  - Index sur `(job_id, ts)` et `(ts)`, avec une rétention bornée (purge périodique, par exemple 7 jours, configurable).
- **Un `slog.Handler` « tee »** qui garde l'écriture sur la sortie standard et recopie de façon **asynchrone**, par lots et sans jamais bloquer le moteur, les enregistrements `WARN` et plus graves vers la table.
  - Les journaux qui portent `job`, `task` et `source` alimentent les colonnes correspondantes.
  - Si la file interne déborde, il abandonne les enregistrements et compte les pertes dans une métrique.
- **Un tampon circulaire en mémoire** pour les journaux `INFO` récents du processus courant (débogage local).
- **Masquage des secrets** avant tout stockage ou exposition : identifiants dans les URL de proxy (`GMAPS_PROXIES` = `http://user:pass@…`), `DATABASE_URL`, clés API, en-têtes `Authorization`. Écris un test.
- **Nouvelles routes REST**, à ajouter aussi dans `openapi.yaml` :
  - `GET /v1/jobs/{id}/tasks?status=failed|pending|running` : diagnostic des tâches.
  - `GET /v1/logs?level=&job_id=&source=&since=&limit=` : réservée à la clé `admin`, car les journaux mélangent tous les clients.
  - Un résumé de santé agrégé, par exemple `GET /v1/health/summary` : base de données, profondeur de file par source, disjoncteurs ouverts, débit adaptatif courant, taux d'erreur récent par source, dernières erreurs. L'agent doit pouvoir répondre à « est-ce que Google nous bloque ? » sans parser du texte Prometheus.
- Étends `store.Store` proprement (méthodes pour les tâches et les événements) dans **les deux** implémentations, `pgstore` et `memstore`.

### 2. Serveur MCP

- Utilise le **SDK Go officiel** `github.com/modelcontextprotocol/go-sdk`. Vérifie la dernière version stable et la version de la spécification MCP qu'elle implémente, et note-les dans la doc.
- **Transport Streamable HTTP**, monté sur `/mcp` dans le même binaire (rôles `api` et `all`, pas `worker`). Même authentification que le REST (Bearer = clé API), même rate-limit, même isolation des jobs par clé. Valide l'en-tête `Origin` comme l'exige la spécification. Respecte les timeouts de `cmd/server` : les flux longs doivent lever la deadline d'écriture comme le font déjà SSE et l'export.
- **Mode stdio** pour les clients locaux (Claude Desktop, Claude Code) : une commande `cmd/mcp` (ou `cleads-extractor mcp`) qui parle à une instance distante **via l'API REST** (`CLEADS_URL`, `CLEADS_API_KEY`) sans accéder à la base, avec la même liste d'outils. Factorise : les outils s'appuient sur une interface commune, implémentée soit en appel direct (in-process), soit par un client HTTP.
- Fournis des **exemples de configuration client** prêts à copier : Claude Desktop (`claude_desktop_config.json`), Claude Code (`claude mcp add …`), et un client HTTP générique.

### 3. Outils MCP

Noms en `snake_case`. Chaque outil a une description claire **orientée usage**, avec ce que l'agent doit en attendre et ses limites, un schéma d'entrée strict, des annotations (`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint`) et des sorties structurées (`structuredContent` + résumé texte court). Minimum attendu :

| Outil | Fait | Notes |
|---|---|---|
| `list_sources`, `list_sectors` | catalogue | lecture seule |
| `create_extraction_job` | `POST /v1/jobs` | expose tous les champs de `JobSpec` ; génère une `Idempotency-Key` si absente et la renvoie ; signale `planned_sources` / `skipped_sources` |
| `get_job`, `list_jobs` | statut, compteurs | |
| `wait_for_job` | attend la fin ou un délai (`timeout_s`, plafonné) | envoie des **notifications de progression MCP** à partir des compteurs ; rend la main avec l'état courant si le délai expire, pour que l'agent puisse relancer |
| `get_job_results` | résultats paginés + filtres | **projection de champs** (`fields`) et mode compact par défaut (sans avis ni photos) pour ne pas saturer le contexte du LLM ; `next_cursor` |
| `export_job` | export CSV / JSONL | ne renvoie jamais un export complet dans le contexte : renvoie un lien de ressource (`resource_link`) ou une URL authentifiée, avec un aperçu de quelques lignes |
| `search_prospect_base`, `get_business` | base consolidée | `get_business` peut inclure les détails complets sur demande |
| `cancel_job` | annulation | `destructiveHint: true` |
| `get_job_errors` | tâches échouées + événements d'erreur du job | regroupe les erreurs par cause (ex. « 12 × http 500 sur `google_maps_http.details` ») |
| `get_logs` | journaux filtrés | clé `admin` uniquement ; erreur explicite sinon |
| `get_system_health` | résumé de santé | réponse directe à « Google nous bloque-t-il ? » |
| `get_stats` | statistiques globales | |

Ajoute aussi :
- **Ressources** MCP : `cleads://openapi`, `cleads://sectors`, `cleads://jobs/{id}` (modèle de ressource), et les exports comme ressources lisibles par morceaux.
- **Un ou deux prompts** MCP utiles, par exemple « campagne de prospection : secteur + zone → job → contrôle qualité → export » et « diagnostiquer un job ».

Mets dans les descriptions des outils les règles métier qu'un LLM doit connaître :
- une seule forme de zone par job ;
- sans `country`, le pays est déduit du lieu (précise-le si le nom est ambigu) ;
- `language` détermine la langue des catégories et des avis ;
- `enrich: true` est plus lent mais trouve les emails ;
- un job dure de quelques secondes à plusieurs minutes, et les résultats sont lisibles en cours de route ;
- `max_results` est un quota exact ;
- par défaut, les commerces sans rapport ajoutés par Google sont filtrés ;
- SIRENE ne couvre que la France.

### 4. Sécurité et limites

- L'isolation actuelle ne doit pas régresser : une clé ne voit que ses jobs, sauf `admin`. Teste-le aussi par MCP.
- Bornes sur tout ce qui peut exploser : `limit` maximum, taille des réponses (ordre de grandeur de quelques dizaines de Ko par appel d'outil), `wait_for_job` plafonné.
- Pas de nouvelle surface SSRF : `webhook_url` passe déjà par le client « IP publiques uniquement » ; ne crée aucun outil qui fasse des requêtes HTTP arbitraires.
- Les avis contiennent des données personnelles de tiers (voir `docs/COMPLIANCE.md`) : mode compact par défaut, sans les avis.
- Aucun secret dans les journaux, les ressources ou les erreurs renvoyées aux agents.

---

## Contraintes de qualité

- **Style du code existant** : commentaires en anglais à la même densité, erreurs enveloppées (`%w`), `gofmt`, `go vet` sans avertissement, aucune variable globale mutable.
- **Tests** (`make test`, `make race`, et `make test-pg` contre une base jetable) :
  - outils MCP testés via le transport en mémoire du SDK, sur `memstore` et la source `mock` ;
  - auth, isolation, `admin` pour les journaux ;
  - projection de champs et bornes de taille ;
  - progression de `wait_for_job` ;
  - masquage des secrets ;
  - tee de journaux non bloquant sous charge ;
  - migration 002 sur PostgreSQL.
  - **Aucun test ne doit appeler Google ni Internet.**
- Vérifie le serveur avec **MCP Inspector** (`npx @modelcontextprotocol/inspector`) en HTTP et en stdio, et décris la procédure dans la doc.
- Ne régresse pas les performances mesurées (`docs/BENCHMARKS.md`) : le tee de journaux ne doit pas ralentir le moteur. Mesure-le avec `cmd/bench` avant et après, et note le résultat.
- Mets à jour : `README.md` (section MCP), `docs/ARCHITECTURE.md`, `docs/RAILWAY.md` (`/mcp` derrière le même domaine), `.env.example` (nouvelles variables : rétention des événements, origines MCP autorisées…) et `openapi.yaml` (nouvelles routes REST). Crée `docs/MCP.md` : outils, exemples de conversation, configuration des clients.
- Fais des commits par étape logique (observabilité, puis serveur MCP, puis stdio, puis docs), sans pousser.

## Livrable final attendu de ta part

Un compte rendu qui contient :
- les versions du SDK et de la spécification MCP utilisées ;
- la liste des outils ;
- le résultat des tests et du benchmark avant/après ;
- ce qui a été vérifié avec MCP Inspector ;
- ce qui reste non vérifié.
