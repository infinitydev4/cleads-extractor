# Déploiement sur un VPS (production) et CI/CD

Cible validée : **OVHcloud VPS-1** (2 vCore, 4 Go, 40 Go NVMe) sous **Debian 13**. Tout autre VPS Debian/Ubuntu avec ≥ 2 Go de RAM convient.

## 1. Architecture sur le serveur

```
Internet ──► :80/:443  Caddy (TLS automatique, HTTP/3)
                         │ reverse proxy (réseau Docker)
                         ▼
                       api ───────────┐
                       worker ────────┼──► postgres (aucun port publié, volume pgdata)
```

| Service | Image | Rôle | Port hôte | Mémoire max |
|---|---|---|---|---|
| `postgres` | `postgres:16-alpine` | base + file de tâches | aucun | 1,25 Go |
| `api` | `ghcr.io/infinitydev4/cleads-extractor` | REST, SSE, MCP (`ROLE=api`) | `127.0.0.1:8080` | 512 Mo |
| `worker` | idem | extraction (`ROLE=worker`) | `127.0.0.1:8081` | 1 Go |
| `caddy` | `caddy:2-alpine` | HTTPS, redirection 80→443 | 80, 443 | 128 Mo |

Durcissement : conteneurs applicatifs en lecture seule, sans capabilities, `no-new-privileges`; seuls 80 et 443 sont publics (Docker contourne `ufw` pour les ports publiés : tout le reste est lié à `127.0.0.1`); journaux Docker limités à 5 × 20 Mo.

Fichiers : [`deploy/prod/`](../deploy/prod) (`docker-compose.yml`, `Caddyfile`, `.env.example`, `bootstrap.sh`, `init-env.sh`, `deploy.sh`, `backup.sh`), copiés dans `/opt/cleads` sur le serveur.

## 2. Première installation

Prérequis : un accès SSH par clé à un compte sudoer, et un nom d'hôte qui pointe vers le serveur (OVH fournit `vps-xxxx.vps.ovh.net`). Les ports 80 et 443 doivent être joignables pour l'obtention du certificat.

```bash
# depuis votre poste : envoyer le code (sans historique git)
git archive HEAD | ssh admin@SERVEUR 'rm -rf /tmp/cleads-src && mkdir /tmp/cleads-src && tar -x -C /tmp/cleads-src'

# sur le serveur
cd /tmp/cleads-src
sudo DEPLOY_PUBKEY="$(cat deploy.pub)" ./deploy/prod/bootstrap.sh      # Docker, pare-feu, fail2ban, utilisateur deploy
sudo cp deploy/prod/* /opt/cleads/ && sudo chown -R deploy:deploy /opt/cleads
sudo -u deploy DOMAIN=vps-xxxx.vps.ovh.net /opt/cleads/init-env.sh      # génère .env avec les secrets

# première image construite sur place (le pipeline prendra le relais ensuite)
sudo docker build -t ghcr.io/infinitydev4/cleads-extractor:bootstrap /tmp/cleads-src
sudo -u deploy SKIP_PULL=1 /opt/cleads/deploy.sh bootstrap
```

Quand la connexion par clé est vérifiée, fermer le mot de passe SSH : `sudo HARDEN_SSH=1 ./deploy/prod/bootstrap.sh` (clé uniquement, root interdit, `00-cleads.conf` prend le pas sur les fichiers cloud-init).

Les clés d'API sont dans `/opt/cleads/.env` (`API_KEYS=admin:…,crm:…`). La clé `admin` voit tous les jobs et les journaux ; `crm` est destinée à l'application métier. Pour brancher des agents LLM (Claude Code, Cursor…) sur `https://<domaine>/mcp`, voir [MCP.md](MCP.md#5-brancher-les-clients-sur-la-production-vps-ovh).

## 3. Pipeline CI/CD (GitHub Actions)

Fichier : [`.github/workflows/ci-cd.yml`](../.github/workflows/ci-cd.yml).

| Événement | Ce qui s'exécute |
|---|---|
| Pull request | `gofmt`, `go vet`, tests avec détecteur de concurrence et PostgreSQL, benchmarks de fumée, build de l'image (sans publication) |
| Push sur `main` | idem + publication de l'image sur GHCR (`sha-<7 car.>` et `latest`) + **déploiement** |
| Manuel (*Run workflow*, `deploy_tag`) | redéploie un tag existant sans reconstruire : **retour arrière** |

Le déploiement : copie du compose et des scripts, puis `deploy.sh <tag>` : pull, redémarrage, attente de `/readyz` (API) et `/healthz` (worker), **retour automatique au tag précédent** si le nouveau ne devient pas sain. Un contrôle public `https://<domaine>/readyz` termine le job. Le jeton GHCR du workflow est transmis par le tube SSH pour le pull et n'est jamais stocké sur le serveur.

### Secrets à configurer (une seule fois)

| Secret | Valeur |
|---|---|
| `VPS_HOST` | IP ou nom d'hôte du serveur |
| `VPS_USER` | `deploy` |
| `VPS_SSH_KEY` | clé privée d'un compte `deploy` dédié (droits : Docker uniquement, pas de sudo) |
| `VPS_KNOWN_HOSTS` | empreintes SSH du serveur (épinglées : `StrictHostKeyChecking yes`) |

Sans ces secrets, le job `deploy` s'arrête avec une simple notice (le CI reste vert). Avec le GitHub CLI authentifié (`gh auth login`) : `./scripts/set-github-secrets.sh`. Sinon : *Settings → Secrets and variables → Actions*.

Le paquet GHCR hérite de la visibilité du dépôt pour la lecture ; le déploiement se connecte avec le jeton du workflow, il n'a donc pas besoin d'être public.

## 4. Exploitation

```bash
cd /opt/cleads
docker compose ps                         # état et santé
docker compose logs -f --tail=100 worker  # journaux (JSON)
curl -s localhost:8080/readyz
./backup.sh                               # sauvegarde immédiate (aussi chaque nuit à 03:15 UTC, 7 jours conservés)
```

- **Avertissements et erreurs** de tous les processus : API `GET /v1/logs` (clé admin), `GET /v1/jobs/{id}/errors`, `GET /v1/health/summary`, ou l'outil MCP `get_system_health`.
- **Restauration** : voir l'en-tête de `backup.sh`. Les sauvegardes restent sur le même disque : copiez-les régulièrement hors du serveur (`rsync`/`rclone` vers un stockage objet). La sauvegarde quotidienne incluse dans l'offre OVH est un instantané du serveur entier, utile en dernier recours.
- **Changer un réglage** (cadence de scraping, proxies…) : éditer `/opt/cleads/.env` puis `docker compose up -d`. Le déploiement ne touche jamais à ce fichier.
- **Rotation d'une clé d'API** : modifier `API_KEYS` dans `.env`, `docker compose up -d api worker`.
- **Mises à jour système** : correctifs de sécurité automatiques (`unattended-upgrades`); redémarrer le serveur de temps en temps pour les noyaux.

### Monter en charge
1. **Cadence** : `SOURCE_RATE=google_maps_http=…` (par worker). Commencez bas (4) et surveillez `cleads_source_requests_total{outcome=~"blocked|rate_limited"}` et `cleads_source_circuit_open` (`GET /metrics`, clé API).
2. **Plus d'adresses IP** : proxies résidentiels (`GMAPS_PROXIES`), ou un second VPS qui exécute uniquement un `worker` pointant vers la même base. Ne publiez jamais PostgreSQL sur Internet : passez par un VPN privé (WireGuard, Tailscale).

## 5. Points d'exploitation à connaître

- **Métriques du moteur** : les compteurs de requêtes (`cleads_source_requests_total`…) vivent dans le **worker**, pas dans l'API. `GET /metrics` sur le domaine public ne renvoie que les métriques HTTP. Le worker est joignable en local sur le serveur : `curl -H "Authorization: Bearer <clé>" http://127.0.0.1:8081/metrics` (pour Prometheus, passez par un tunnel SSH ou un VPN, jamais en public). La synthèse prête à lire, tirée de la base, est `GET /v1/health/summary` (clé admin).
- **SSH** : le script d'installation laisse l'authentification par mot de passe active tant qu'aucune clé n'est installée (protection : `fail2ban` + `ufw limit`). Dès que votre clé est dans `~/.ssh/authorized_keys` du compte `debian` (`ssh-copy-id`), fermez le mot de passe : `sudo HARDEN_SSH=1 /opt/cleads/bootstrap.sh`, et vérifiez la connexion par clé **dans un second terminal avant de fermer le premier**.
- **Limite de connexions SSH** : `ufw limit 22` refuse une IP qui ouvre 6 connexions en 30 s. Les scripts enchaînés doivent réutiliser une connexion (`ControlMaster auto`, déjà fait dans le pipeline).
- **CPU** : le crawl des sites web (enrichissement) est limité par le CPU (expressions régulières sur des pages jusqu'à 1 Mo). Sur 2 vCPU, `ENRICH_CONCURRENCY=8` par défaut ; mesuré à 32 : 130 à 185 % de CPU soutenus par le worker. Piste d'optimisation : préfiltrer les pages avant les expressions régulières.
- **Sauvegardes** : elles restent sur le même disque (`/opt/cleads/backups`). Copiez-les hors du serveur.

## 6. Dépannage

| Symptôme | Piste |
|---|---|
| Pas de certificat HTTPS | `docker compose logs caddy` ; ports 80/443 ouverts chez l'hébergeur ; le DNS du nom d'hôte pointe bien vers le serveur. Let's Encrypt limite les émissions par domaine parent (`ovh.net` est partagé) : Caddy bascule seul sur ZeroSSL. |
| Déploiement en échec, retour arrière | la sortie de `deploy.sh` (dans le job GitHub) contient l'état de `compose ps` et les 80 dernières lignes des journaux |
| `outcome="blocked"` en hausse | Google limite cette IP : baisser `SOURCE_RATE`, ajouter des proxies ; le disjoncteur de la source se rouvre seul avec un délai croissant (30 s → 30 min) |
| Disque plein | `docker system df` ; les images de plus de 7 jours non utilisées sont purgées à chaque déploiement ; vérifier `backups/` |
