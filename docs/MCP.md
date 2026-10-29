# Serveur MCP

Le serveur MCP (Model Context Protocol) permet à un agent LLM (Claude Code, Claude Desktop, Cursor, agent maison) de **tout faire avec l'API** : lancer une extraction, la suivre, lire et exporter les résultats, interroger la base consolidée et diagnostiquer seul un job qui échoue.

| | |
|---|---|
| SDK | [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) **v1.8.0** (dernière version stable au 29/10/2026) |
| Spécification MCP | **2026-07-28** (version la plus récente prise en charge par le SDK) ; négocie aussi 2025-11-25, 2025-06-18, 2025-03-26 et 2024-11-05 pour les clients plus anciens |
| Transports | **Streamable HTTP** sur `/mcp` (rôles `api` et `all`) · **stdio** (`cleads-mcp` ou `cleads-extractor mcp`) |

## 1. Architecture

```
Claude Desktop / Code ──stdio──► cleads-mcp ──REST (CLEADS_URL, clé API)──┐
                                                                          ▼
Agent HTTP ──Streamable HTTP /mcp──► api (même processus, même auth) ──► backend.Backend ──► store, moteur
```

Les outils ne dépendent que de l'interface `internal/backend.Backend`, implémentée deux fois :

- **en direct** (`api.Server.Backend(cléID)`) derrière `/mcp` : appels au store et au moteur, sans saut réseau ;
- **par HTTP** (`internal/apiclient`) dans le mode stdio : le binaire local parle à une instance distante par son API REST et **n'accède jamais à la base**.

Les routes REST appellent elles aussi `api.Server.Backend`. Le REST, `/mcp` et le stdio appliquent donc les mêmes règles : validation, isolation des jobs par clé, droits `admin`.

### Transport HTTP
- **Sans état** (`Stateless: true`) : aucune session n'est gardée entre deux requêtes, donc n'importe quel réplica de l'API sert n'importe quel appel, sans affinité de session derrière Caddy, le répartiteur de Railway ou tout autre proxy. Les notifications de progression passent dans le flux SSE de la réponse de l'appel qui les émet. `GET` et `DELETE /mcp` répondent 405.
- **Authentification** : la même que le REST (`Authorization: Bearer <clé>` ou `X-API-Key`), avec le même rate-limit par clé. Un appel d'outil compte comme une requête. L'identité de la clé est transmise au SDK par `auth.TokenInfo`.
- **Origin** : une requête qui porte un en-tête `Origin` hors de `MCP_ALLOWED_ORIGINS` ∪ `CORS_ORIGINS`, et qui n'est pas de même origine, est refusée en 403, comme l'exige la spécification contre le DNS rebinding. Les clients non-navigateurs n'envoient pas d'`Origin` et ne sont pas concernés. La protection « localhost » du SDK reste active.
- **Timeouts** : `cmd/server` coupe les réponses à 60 s (`WriteTimeout`). Pour `/mcp`, la deadline d'écriture est relevée à `MCP_MAX_WAIT + 30 s` et reste bornée, comme pour le SSE et l'export. Vérifié avec un `wait_for_job` de 75 s.

## 2. Outils

Tous les outils renvoient un `structuredContent` (schéma de sortie publié) et deux blocs texte : un **résumé d'une ligne**, puis le même JSON pour les clients qui ne lisent que le texte, comme le recommande la spécification. Les schémas d'entrée sont **stricts** : une propriété inconnue est rejetée, et les énumérations et bornes sont déclarées. Aucun outil n'est `openWorld` : ils n'appellent que l'API CLEADS. Aucun ne fait de requête HTTP arbitraire.

| Outil | Rôle | Annotations | Limites |
|---|---|---|---|
| `list_sources` | sources actives et profil | lecture seule | |
| `list_sectors` | taxonomie (clé → type Google, tags OSM, NAF) | lecture seule | |
| `create_extraction_job` | `POST /v1/jobs`, tous les champs de `JobSpec` | écriture, non idempotent | génère une `idempotency_key` si absente et la renvoie ; `replayed: true` quand la clé existait ; `planned_sources` / `skipped_sources` |
| `get_job` | statut, compteurs, avancement | lecture seule | |
| `list_jobs` | jobs de la clé | lecture seule | 50 par page |
| `wait_for_job` | attend la fin (ou `min_unique` fiches) | lecture seule | `timeout_s` ≤ `MCP_MAX_WAIT` (120 s) ; **notifications de progression** (`progress` = tâches terminées, `total` = tâches connues) ; rend la main avec `timed_out: true` |
| `get_job_results` | résultats paginés + filtres | lecture seule | **mode compact par défaut** (sans avis, photos ni identifiants bruts) ; `fields` pour projeter ; 100 fiches et ~24 Ko max (`truncated: true` et `next_cursor` ajusté) |
| `export_job` | export CSV / csv-excel / JSONL | lecture seule | ne renvoie **jamais** l'export complet : un `resource_link` lisible par morceaux, une URL de téléchargement authentifiée et un aperçu de 5 lignes |
| `search_prospect_base` | base consolidée | lecture seule | mêmes bornes que `get_job_results` |
| `get_business` | une fiche | lecture seule | `include_details` (profil complet), `include_reviews` (3 avis courts) ; réduite si > 24 Ko |
| `cancel_job` | annulation | **destructif**, idempotent | `conflict` si le job est déjà terminé |
| `get_job_errors` | tâches échouées, tâches en reprise et événements d'erreur du job, **regroupés par cause** | lecture seule | ex. « 12 × http 500 on google_maps_http.details (failed) » |
| `get_logs` | journaux WARN+ de tous les processus | lecture seule | **clé `admin` uniquement** (erreur `forbidden` sinon) ; 200 événements et ~24 Ko max ; `local: true` lit le tampon INFO du réplica qui répond |
| `get_system_health` | résumé de santé | lecture seule | répond directement à « Google nous bloque-t-il ? » ; dernières erreurs pour `admin` seulement |
| `get_stats` | statistiques globales | lecture seule | |

Les règles métier figurent dans les descriptions des outils et dans les `instructions` du serveur : une seule forme de zone par job, pays déduit du lieu, `language`, `enrich` plus lent mais seul moyen d'obtenir des emails, durée et résultats lisibles en cours de route, `max_results` exact, filtrage des commerces sans rapport, SIRENE réservé à la France.

Les erreurs renvoyées à l'agent sont des codes stables (`not_found`, `invalid_spec`, `conflict`, `forbidden`, `rate_limited`, `unavailable`…) suivis d'un message. Une erreur interne inattendue est journalisée côté serveur et n'apparaît que sous la forme `internal_error`, sans détail. Les secrets sont masqués partout.

## 3. Ressources et prompts

| URI | Contenu |
|---|---|
| `cleads://openapi` | spécification OpenAPI (YAML) |
| `cleads://sectors` | taxonomie (JSON) |
| `cleads://jobs/{id}` | job, compteurs et avancement (modèle) |
| `cleads://jobs/{id}/export/{format}{?cursor,filter}` | export **par morceaux** : 500 lignes et 48 Ko au plus par lecture, en-tête CSV dans le premier morceau seulement ; `_meta.next_uri` donne le morceau suivant et `_meta.done` signale la fin |

Prompts :
- `prospecting_campaign` (`activity`, `area`, options `country`, `language`, `emails`, `max_results`) : secteur et zone → job → attente → contrôle qualité → export ;
- `diagnose_job` (`job_id`) : statut → erreurs groupées → santé des sources → journaux → conclusion.

## 4. Exemple de conversation

> **Utilisateur** : Cherche les plombiers de Lyon avec leurs emails, attends la fin, donne-moi les 50 meilleures fiches, puis explique pourquoi 3 tâches ont échoué.

1. `create_extraction_job {"sector":"plumber","location":"Lyon, France","enrich":true}` → `job_…`, sources `google_maps_http, osm, sirene`.
2. `wait_for_job {"job_id":"job_…","timeout_s":120}` → `timed_out: true` (1 240 uniques), puis un second appel → `finished`.
3. `get_job_results {"job_id":"job_…","limit":50,"useful":true,"has_email":true,"min_completeness":80}` → 50 fiches compactes.
4. `get_job_errors {"job_id":"job_…"}` → « 3 × http 500 on google_maps_http.details (failed) ».
5. `get_system_health {}` → « no source is blocked; google_maps_http: ok — 412 requests in 5m0s… ». Conclusion : erreurs transitoires de Google sur 3 fiches, pas de blocage.
6. `export_job {"job_id":"job_…","format":"csv-excel","useful":true}` → lien de ressource et URL de téléchargement.

## 5. Brancher les clients sur la production (VPS OVH)

L'instance de production tourne sur le VPS OVH décrit dans [DEPLOY_VPS.md](DEPLOY_VPS.md). Caddy sert le MCP en HTTPS (certificat automatique) :

```
https://vps-xxxxxxxx.vps.ovh.net/mcp        ← remplacer par le DOMAIN de /opt/cleads/.env
```

L'URL réelle et les clés sont dans `/opt/cleads/.env` (`DOMAIN`, `API_KEYS=admin:…,crm:…`). Dans les exemples ci-dessous, `$CLEADS_URL` vaut `https://vps-xxxxxxxx.vps.ovh.net` et `$CLEADS_API_KEY` la clé choisie.

### Quelle clé donner à quel agent ?

| Clé | Voit | Utiliser pour |
|---|---|---|
| `crm` | seulement ses propres jobs ; `get_logs` refusé (`forbidden`) | agents de prospection au quotidien |
| `admin` | tous les jobs, `get_logs`, dernières erreurs dans `get_system_health` | agent d'exploitation qui diagnostique la production |

Donnez une clé par agent ou par outil (`API_KEYS=admin:…,crm:…,cursor:…`, puis `docker compose up -d api worker`) : on retire l'accès d'un client sans toucher aux autres. Ne mettez jamais une clé dans un dépôt : utilisez une variable d'environnement.

### Tableau de compatibilité

| Client | Transport | Authentification par clé API | Statut |
|---|---|---|---|
| **Claude Code** | HTTP direct ou stdio | `--header` | ✅ vérifié (HTTP et stdio, contre la production) |
| **Claude Desktop** | stdio (`cleads-mcp`) | variables d'environnement | ✅ stdio vérifié contre la production |
| **Claude.ai** (connecteur personnalisé) | HTTP, appelé **depuis le cloud d'Anthropic** | *Request headers* (bêta) | ⚠️ non essayé : l'option n'est pas ouverte à tous les comptes |
| **Cursor** | HTTP direct ou stdio | `headers` dans `mcp.json` | ✅ requêtes équivalentes vérifiées avec curl ; Cursor lui-même non lancé |
| **ChatGPT** (connecteur) | HTTP, appelé depuis le cloud d'OpenAI | ❌ OAuth ou aucune authentification seulement | ⛔ **pas branchable en l'état** (voir plus bas) |

« Vérifié » signifie : appel réel contre `https://<domaine>/mcp` le 29/10/2026. Les 15 outils sont listés avec l'en-tête `Authorization: Bearer` comme avec `X-API-Key`; sans clé la réponse est 401; un en-tête `Origin` étranger donne 403; la clé `crm` reçoit `forbidden` sur `get_logs` et la clé `admin` obtient `get_system_health`.

### Claude Code

```bash
export CLEADS_URL=https://vps-xxxxxxxx.vps.ovh.net
export CLEADS_API_KEY=<clé crm ou admin>

# HTTP direct (recommandé : rien à installer)
claude mcp add --transport http cleads "$CLEADS_URL/mcp" --header "Authorization: Bearer $CLEADS_API_KEY"

# vérifier : « ✔ Connected » attendu
claude mcp list
```

Portée : `--scope user` pour tous vos projets, `--scope project` pour écrire `.mcp.json` et le partager avec l'équipe. Dans ce second cas, **ne mettez pas la clé en clair** : référencez une variable.

```json
{
  "mcpServers": {
    "cleads": {
      "type": "http",
      "url": "${CLEADS_URL}/mcp",
      "headers": { "Authorization": "Bearer ${CLEADS_API_KEY}" }
    }
  }
}
```

Variante stdio (binaire local `bin/cleads-mcp`, obtenu avec `make build`) :

```bash
claude mcp add cleads --env CLEADS_URL=$CLEADS_URL --env CLEADS_API_KEY=$CLEADS_API_KEY -- /chemin/vers/bin/cleads-mcp
```

Si `claude mcp list` affiche `✘ Failed to connect`, `claude mcp get cleads` donne le code HTTP : 401 = clé fausse, 403 = en-tête `Origin` refusé.

### Claude Desktop (`claude_desktop_config.json`)

Claude Desktop lance un processus local : utilisez le mode **stdio**, qui parle à la production par son API REST.

```json
{
  "mcpServers": {
    "cleads": {
      "command": "/chemin/vers/bin/cleads-mcp",
      "env": {
        "CLEADS_URL": "https://vps-xxxxxxxx.vps.ovh.net",
        "CLEADS_API_KEY": "<votre clé API>"
      }
    }
  }
}
```

Sans compiler : l'image de production contient le même binaire (`docker login ghcr.io` d'abord si le paquet est privé).

```json
{
  "mcpServers": {
    "cleads": {
      "command": "docker",
      "args": ["run", "-i", "--rm", "-e", "CLEADS_URL", "-e", "CLEADS_API_KEY", "ghcr.io/infinitydev4/cleads-extractor:latest", "mcp"],
      "env": { "CLEADS_URL": "https://vps-xxxxxxxx.vps.ovh.net", "CLEADS_API_KEY": "<votre clé API>" }
    }
  }
}
```

Variables du mode stdio : `CLEADS_URL` et `CLEADS_API_KEY` (obligatoires), `CLEADS_MCP_MAX_WAIT` (plafond de `wait_for_job`, 120 s par défaut). Les journaux du mode stdio sortent sur stderr, clé masquée. Redémarrez Claude Desktop après modification.

### Claude.ai (connecteur personnalisé)

Réglages → Connecteurs → *Ajouter un connecteur personnalisé* :

1. URL du serveur MCP : `https://vps-xxxxxxxx.vps.ovh.net/mcp`.
2. Authentification : **No sign-in**. L'option est trompeuse : elle signifie « pas d'OAuth », la clé est envoyée par l'en-tête.
3. *Request headers* → nom `x-api-key` (dans la liste proposée, et accepté par notre serveur), valeur = la clé, **requis**.

Points à connaître : l'option *Request headers* est en **bêta, réservée à certaines organisations** (si elle n'apparaît pas, ce compte ne peut pas brancher ce serveur à clé statique) ; l'authentification **ne se modifie pas** après création (supprimer puis recréer pour changer de clé) ; la clé est partagée par tous les membres de l'organisation ; les requêtes partent du **cloud d'Anthropic**, donc le VPS doit rester joignable depuis Internet sur 443 (c'est le cas). Source : [documentation des connecteurs personnalisés](https://claude.com/docs/connectors/custom/remote-mcp). Cette configuration n'a pas été essayée ici.

### Cursor

Fichier `~/.cursor/mcp.json` (tous les projets) ou `.cursor/mcp.json` (un projet). Cursor résout `${env:NOM}` dans `url` et `headers` : la clé reste dans votre environnement, pas dans le fichier.

```json
{
  "mcpServers": {
    "cleads": {
      "url": "https://vps-xxxxxxxx.vps.ovh.net/mcp",
      "headers": { "Authorization": "Bearer ${env:CLEADS_API_KEY}" }
    }
  }
}
```

Variante stdio :

```json
{
  "mcpServers": {
    "cleads": {
      "type": "stdio",
      "command": "/chemin/vers/bin/cleads-mcp",
      "env": { "CLEADS_URL": "https://vps-xxxxxxxx.vps.ovh.net", "CLEADS_API_KEY": "${env:CLEADS_API_KEY}" }
    }
  }
}
```

Lancez Cursor depuis un terminal où `CLEADS_API_KEY` est exportée (une application ouverte depuis le Dock ne voit pas les variables de votre shell). Un rapport du forum Cursor signale que certains en-têtes personnalisés seraient perdus sur `tools/call` en Streamable HTTP : si `tools/list` marche mais pas les appels, passez à la variante stdio, qui n'a pas ce problème.

### ChatGPT : pas branchable en l'état

Le connecteur personnalisé de ChatGPT ([documentation OpenAI](https://developers.openai.com/api/docs/guides/custom-mcp-server)) ne propose que **OAuth**, **aucune authentification** ou un mode mixte. Il n'envoie pas de clé statique en en-tête, et il n'exécute pas de processus local (pas de stdio). Notre serveur n'accepte qu'une clé statique et n'implémente pas OAuth. Conséquences :

- **Ne choisissez pas « aucune authentification »** : l'URL est publique, et ce serveur lance des extractions et expose des coordonnées d'entreprises. Il répondrait 401, mais ne contournez jamais cette protection.
- Pour ChatGPT, il faut **ajouter OAuth au serveur** (métadonnées de ressource protégée, serveur d'autorisation ou fournisseur d'identité externe). C'est un chantier à part, non fait.
- Par **programmation**, l'outil MCP distant de l'API OpenAI (Responses API) accepte un jeton et des en-têtes de la part de votre code : `{"type":"mcp","server_label":"cleads","server_url":"https://…/mcp","authorization":"<clé>"}`. C'est le chemin à privilégier pour un agent maison fondé sur les modèles OpenAI. **Non essayé ici** (pas de clé OpenAI).

### Client HTTP générique

Point d'accès `POST https://<domaine>/mcp`, en-têtes `Authorization: Bearer <clé>` (ou `X-API-Key: <clé>`), `Content-Type: application/json`, `Accept: application/json, text/event-stream`. La réponse est un flux SSE qui contient les éventuelles notifications puis le résultat.

```bash
curl -N "$CLEADS_URL/mcp" -H "Authorization: Bearer $CLEADS_API_KEY" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_system_health","arguments":{}}}'
```

En Go : `mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: <client qui ajoute l'en-tête>}`. Voir `internal/api/mcp_test.go`.

### Particularités de la production

- **Délais** : Caddy ne met pas les flux MCP en tampon ni ne les compresse (`flush_interval -1`) ; `wait_for_job` rend la main avec `timed_out: true` avant 120 s et l'agent rappelle l'outil. Pendant un redémarrage de l'API (déploiement), un appel en cours échoue et l'agent le répète. Pour `create_extraction_job`, l'agent doit réutiliser l'`idempotency_key` renvoyée à la première réponse : sans elle, une répétition créerait un second job.
- **Origin** : les clients de bureau et les agents n'envoient pas d'`Origin`. Un client **web** hébergé sur un autre domaine doit être déclaré dans `MCP_ALLOWED_ORIGINS` (`/opt/cleads/.env`, puis `docker compose up -d api`), sinon 403.
- **Rotation** : changer une clé dans `API_KEYS` coupe immédiatement le client concerné ; mettez à jour sa configuration (Claude.ai : supprimer et recréer le connecteur).
- **Surveillance** : un agent avec la clé `admin` peut répondre à « la production va-t-elle bien ? » avec `get_system_health` et `get_logs`, sans accès SSH.

## 6. Vérifier avec MCP Inspector

```bash
# 1. une instance locale, sans Internet (source synthétique, base en mémoire)
APP_ENV=development AUTH_DISABLED=true ENABLE_MOCK_SOURCE=true ENABLE_GMAPS_SCRAPING=false \
  ENABLE_OSM=false ENABLE_SIRENE=false ENRICH_ENABLED=false GEOCODER=none PORT=8080 go run ./cmd/server
```

```bash
# 2a. HTTP : interface web, transport « Streamable HTTP », URL http://localhost:8080/mcp,
#     en-tête Authorization: Bearer <clé> si l'authentification est active
npx @modelcontextprotocol/inspector
```

```bash
# 2b. stdio : l'Inspector lance le binaire
make build
npx @modelcontextprotocol/inspector -e CLEADS_URL=http://localhost:8080 -e CLEADS_API_KEY=<clé> ./bin/cleads-mcp
```

```bash
# 2c. sans interface (CLI) : liste des outils, puis un appel
npx @modelcontextprotocol/inspector --cli http://localhost:8080/mcp --transport http --method tools/list
npx @modelcontextprotocol/inspector --cli ./bin/cleads-mcp -e CLEADS_URL=http://localhost:8080 -e CLEADS_API_KEY=<clé> \
  --method tools/call --tool-name get_system_health
```

> **Vérifié le 29/10/2026 avec MCP Inspector 2.13.0 (CLI)**, contre le binaire réel, PostgreSQL et la source `mock`.
> - **HTTP** : `tools/list` (15 outils) ; enchaînement `create_extraction_job` → `wait_for_job` (`finished`, 2 280 uniques) → `get_job_results` avec `fields` → `export_job` (blocs texte et `resource_link`) ; `resources/list`, `resources/templates/list` ; `resources/read` d'un morceau d'export (209 lignes, 49 Ko, `_meta.next_uri`) et d'un job ; `prompts/list` et `prompts/get`.
> - **stdio** : l'Inspector lance `bin/cleads-mcp` et obtient la même liste d'outils. `get_job_errors`, `get_system_health` (clé admin) et la lecture d'un export JSONL fonctionnent ; `get_logs` avec une clé non admin renvoie `forbidden`.
> - **Portabilité des schémas** (`--strict`) : aucun avertissement sur les schémas d'entrée. Ils sont mono-type, car certains dialectes de déclaration de fonctions refusent `["null", …]`. Il reste 29 avertissements, tous sur des schémas de **sortie** : les types `["null", "array"|"object"]` des slices, maps et pointeurs Go y sont exacts, et le SDK valide chaque résultat contre eux.
>
> La progression de `wait_for_job` a été vérifiée séparément : 77 notifications pendant un appel de 75 s, en JSON-RPC brut avec curl, car le mode CLI de l'Inspector ne les affiche pas. L'interface web de l'Inspector n'a pas été ouverte.

À contrôler : les 15 outils, avec leurs annotations et leurs schémas ; `create_extraction_job` avec `{"query":"plombier","center":{"lat":45.76,"lng":4.84},"radius_m":3000}`, puis `wait_for_job` (les notifications de progression s'affichent dans le panneau) ; `export_job`, puis la lecture de la ressource liée et de ses `next_uri` ; les 4 modèles et ressources ; les 2 prompts.

## 7. Sécurité et limites

- **Isolation** : une clé ne voit que ses jobs (`not_found` pour ceux des autres), sauf `admin`. C'est testé par MCP en mémoire, sur HTTP et en stdio.
- **Journaux** : réservés à `admin`, puisqu'ils mélangent les clients. Les autres clés passent par `get_job_errors`, limité à leurs jobs.
- **Bornes** : pages de 100 fiches au plus et ~24 Ko par résultat d'outil ; 200 événements de journal ; morceaux d'export de 500 lignes et 48 Ko ; `wait_for_job` ≤ `MCP_MAX_WAIT` ; corps de requête ≤ `MAX_BODY_BYTES`.
- **Données personnelles** : le mode compact exclut les avis, qui contiennent des données personnelles de tiers (voir [COMPLIANCE.md](COMPLIANCE.md)). `include_reviews` en donne au plus 3 par fiche, tronqués à 300 caractères, sans lien vers le profil de l'auteur.
- **SSRF** : aucun nouvel accès réseau sortant. `webhook_url` passe toujours par le client « IP publiques uniquement », et le mode stdio n'appelle que `CLEADS_URL`.
- **Secrets** : masqués dans les journaux, les événements, les erreurs de tâches et les réponses d'outils (identifiants de proxy, `DATABASE_URL`, clés API, en-têtes `Authorization`).
