# Déploiement sur Railway

Topologie recommandée : **PostgreSQL** + service **api** + service **worker** (scalable), et optionnellement **gmaps-scraper** (navigateur). Tout tourne sur la même image Docker ; `ROLE` choisit le comportement.

## 1. Base de données
1. *New → Database → PostgreSQL*.
2. Budget de connexions : `DB_MAX_CONNS × (réplicas api + réplicas worker)` doit rester sous `max_connections` du plan (par défaut 100 : gardez `DB_MAX_CONNS=20` et ≤ 4 réplicas au total, ou montez la limite).

## 2. Service `api`
1. *New → GitHub Repo* → ce dépôt (le `railway.json` racine est pris en compte : build Dockerfile, healthcheck `/readyz`, drainage 30 s).
2. Variables :
   ```
   ROLE=api
   DATABASE_URL=${{Postgres.DATABASE_URL}}
   API_KEYS=admin:<clé aléatoire ≥ 24 car.>,crm:<autre clé>
   WEBHOOK_SECRET=<secret>
   GOOGLE_PLACES_API_KEY=<clé>          # si Places est utilisé (sert aussi au géocodage)
   GEOCODER_USER_AGENT=CLEADS-Business-Extractor/1.0 (contact@votre-domaine.fr)
   ```
3. *Settings → Networking → Generate Domain* pour l'exposer.

Générer une clé : `openssl rand -base64 32`.

## 3. Service `worker`
1. *New → GitHub Repo* → même dépôt.
2. *Settings → Config-as-code* : chemin `deploy/railway.worker.json` (healthcheck `/healthz`).
3. Variables : les mêmes que l'API avec `ROLE=worker` (`API_KEYS` n'y sert qu'à protéger `/metrics` si vous le scrapez), plus les réglages moteur :
   ```
   ROLE=worker
   DATABASE_URL=${{Postgres.DATABASE_URL}}
   ENRICH_ENABLED=true
   SOURCE_CONCURRENCY=google_places=16,osm=2,sirene=4
   SOURCE_RATE=google_places=8
   ```
4. *Settings → Replicas* pour scaler horizontalement. Les débits `SOURCE_RATE` sont **par worker** : divisez le budget fournisseur par le nombre de réplicas.

> Petit volume : un seul service avec `ROLE=all` suffit.

## 4. Service `gmaps-scraper` (optionnel, source `google_maps_browser`)
1. *New → Docker Image* : `gosom/google-maps-scraper:latest`.
2. *Custom Start Command* : `-web -data-folder /gmapsdata -c 2`, port 8080, volume monté sur `/gmapsdata`.
3. Ne **pas** générer de domaine public. Sur le worker : `GMAPS_SCRAPER_URL=http://${{gmaps-scraper.RAILWAY_PRIVATE_DOMAIN}}:8080`.
4. Prévoir au moins 2 Go de RAM pour ce service (Chromium). Lisez d'abord `docs/COMPLIANCE.md`.

## 5. Dimensionnement de départ

| Service | vCPU | RAM | Remarque |
|---|---|---|---|
| api | 0,5 | 256 Mo | 51 Mo RSS mesurés sous charge |
| worker | 1 | 512 Mo | 31–65 Mo mesurés ; marge pour l'enrichissement et le cache de dédup (`DEDUP_CACHE_SIZE`) |
| PostgreSQL | 1–2 | 1–2 Go | c'est le goulot mesuré du moteur |
| gmaps-scraper | 2 | 2–4 Go | uniquement si utilisé |

`GOMEMLIMIT` est fixé automatiquement à 90 % de la limite mémoire du conteneur.

## 6. Exploitation
- **Santé** : `/healthz` (vivant), `/readyz` (base joignable).
- **Métriques** : `GET /metrics` (authentifié) : `cleads_records_{raw,unique,complete,crm_useful}_total`, latence et issues par source, débit adaptatif courant, état des disjoncteurs, profondeur de file.
- **Profilage** : `PPROF_ADDR=127.0.0.1:6060` (jamais exposé publiquement).
- **Déploiements** : SIGTERM → arrêt de la location de tâches, fin des tâches en cours (`SHUTDOWN_WINDOW`), sinon remise en file immédiate. Aucun travail perdu.
- **Migrations** : appliquées au démarrage sous verrou consultatif (sans risque avec plusieurs réplicas).

## 7. Valider la capacité après déploiement
```bash
railway run --service worker -- go run ./cmd/bench -store pg -dsn "$DATABASE_URL" -workers 2 -area-km 20
```
Puis un job réel sur une ville connue, en suivant `/v1/jobs/{id}/events` et `/metrics`.
