# Conformité et usage des données

Ce document n'est pas un avis juridique. Il liste les points que l'opérateur doit vérifier avant d'utiliser chaque source en production.

## Sources et licences

| Source | Statut | Ce que le code fait par défaut |
|---|---|---|
| **Google Places API** (`google_places`) | Officielle, sous contrat Google Maps Platform. Les conditions limitent la mise en cache du contenu (les `place_id` peuvent être conservés ; les autres champs ont des durées de conservation limitées) et imposent l'attribution Google à l'affichage. | Activée si `GOOGLE_PLACES_API_KEY` est défini. Vérifiez les conditions en vigueur et prévoyez une purge/actualisation des champs Places si vous constituez une base durable. |
| **OpenStreetMap** (`osm`) | Données ODbL : réutilisation libre, **attribution « © les contributeurs d'OpenStreetMap »** obligatoire, partage à l'identique si vous redistribuez publiquement une base dérivée. Usage raisonnable des instances Overpass publiques. | Activée. User-Agent identifiable, débit limité, bascule sur miroirs. Au-delà de quelques milliers de requêtes/jour : instance privée. |
| **SIRENE** (`sirene`) | Licence Ouverte Etalab 2.0 (attribution). Les personnes en **diffusion partielle** (`statut_diffusion = P`) se sont opposées à la réutilisation de leurs données. | Activée. Établissements en diffusion partielle **exclus**, comme les établissements fermés. |
| **Google Maps HTTP** (`google_maps_http`) | Non officielle. Les conditions d'utilisation de Google interdisent l'accès automatisé à Google Maps hors API. | **Désactivée** (`ENABLE_GMAPS_SCRAPING=false`). L'activation relève de la responsabilité de l'opérateur. |
| **Google Maps navigateur** (`google_maps_browser`) | Même statut, via le sidecar `gosom/google-maps-scraper` (licence MIT pour le code). | Désactivée tant que `GMAPS_SCRAPER_URL` n'est pas défini. |
| **Sites des entreprises** (enrichissement) | Pages publiques. | robots.txt respecté, ≤ 4 pages par site, 1 requête simultanée par hôte, User-Agent identifiable, adresses privées refusées (anti-SSRF). |

Le code issu de `gosom/google-maps-scraper` (chemins de parsing, paramètre `pb`, fixtures de test) est sous licence MIT ; l'attribution figure dans les fichiers concernés.

## RGPD et prospection B2B (France)

Les fiches d'entreprises individuelles, d'indépendants et les adresses email nominatives (`prenom.nom@…`) sont des **données personnelles**. Points d'attention (voir les recommandations de la CNIL sur la prospection commerciale) :

- **Base légale** : l'intérêt légitime est généralement admis pour la prospection B2B, à condition que le message soit en rapport avec l'activité professionnelle du destinataire.
- **Information et opposition** : informer les personnes de la collecte et de la source à la première prise de contact, et offrir un moyen simple de s'opposer. L'opposition doit être respectée dans toutes les extractions futures (prévoir une liste d'exclusion côté CRM).
- **Minimisation** : n'extraire que les champs utiles ; les avis et photos ne sont pas collectés.
- **Durée de conservation** : définir une durée (la CNIL recommande 3 ans après le dernier contact pour un prospect) et purger.
- **Registre des traitements** : y inscrire ce traitement, ses sources et ses destinataires.
- **Bloctel** ne s'applique qu'aux consommateurs, mais les entrepreneurs individuels peuvent y être inscrits : à vérifier avant toute prospection téléphonique.

## Sécurité
- Clés API stockées hachées (SHA-256), comparées en temps constant, rate-limit par clé, isolation des jobs par clé (sauf clé `admin`).
- Webhooks signés `HMAC-SHA256(secret, timestamp + "." + corps)` ; destinations privées refusées.
- Export CSV protégé contre l'injection de formules.
- Image distroless non-root ; aucun secret journalisé (`Config.Redacted`).
