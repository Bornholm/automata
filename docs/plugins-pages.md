# Plugin Pages

Le plugin `pages` donne à Automata un **atelier de pages web** : un membre
demande dans la conversation une page d'invitation, une page photo, un
mini-site — l'assistant la construit dans un **brouillon privé**, puis, sur
« confirmer », la publie sous un **lien public court** (`/s/<slug>/`) que le
membre partage. Les images et vidéos envoyées dans la conversation sont
réutilisables dans les pages.

Le profil web du membre gagne un onglet « pages » : liste des espaces,
lien à copier, téléchargement d'une archive zip, suppression.

## Architecture

Le plugin ne stocke rien lui-même : les fichiers vivent dans le **magasin
d'objets des plugins**, côté hôte (tables `plugin_objects` et
`plugin_public_sites`, migration 0021), exposé par les RPC génériques du
service hôte (`PutObject`, `GetObject`, `CopyCollection`,
`PublishCollection`…). Tout futur plugin peut s'en servir.

Contrairement à `plugin_configs` et `plugin_secrets`, les objets ne sont
**pas scellés au repos** : ce magasin porte du contenu destiné à être servi
publiquement — il ne doit JAMAIS recevoir de secret.

Un espace `<nom>` occupe deux collections :

- `spaces/<nom>/draft` — le brouillon, seul endroit où les outils écrivent ;
- `spaces/<nom>/live` — la copie publiée, créée par `publish_space`
  (copie transactionnelle draft → live).

Modifier une page déjà publiée ne change donc **rien en ligne** avant le
prochain `publish_space`, lui-même confirmé.

Le membre peut **prévisualiser son brouillon sans confirmation** :
`preview_space` (outil `read_only`) rend un lien signé éphémère
(`/d/<jeton>/`, TTL 1 h — le jeton voyage dans l'URL, donc dans les
journaux du proxy, même raisonnement que les jetons d'iframe) servi avec
les mêmes en-têtes d'isolement que `/s/` mais en `Cache-Control:
no-store`. C'est une capacité personnelle remise dans le canal privé —
rien ne devient public, d'où l'absence de confirmation. Le secret de
signature reste côté hôte (`web.DraftPreviewMinter`, câblé au service
hôte par le registre) ; l'UI de profil affiche le même lien, refabriqué
à chaque affichage.

Les pièces jointes importées (`import_attachment`) atterrissent dans la
collection `imports`, puis `use_file` les place dans un espace.

## Sécurité

- **Écritures de brouillon en `read_only`** — décision assumée, sur le
  précédent du plugin `workspace` : `create_space`, `write_file`,
  `delete_file` et `use_file` ne touchent que le brouillon privé du membre,
  invisible du public. La frontière de sécurité est l'**exposition
  publique** : `publish_space`, `unpublish_space` et `delete_space` sont
  des écritures et passent par la confirmation humaine (« confirmer »).
  Corollaire : aucun outil de brouillon ne doit jamais toucher une
  collection `live`.
- **La route publique `/s/{slug}/…`** (hôte, `handlers_public_site.go`)
  traite le HTML comme hostile : `Content-Security-Policy: sandbox
  allow-scripts` (origine opaque — un script de la page ne peut ni lire de
  cookie ni émettre de requête créditée vers `/admin` ou `/p/`), `nosniff`,
  `Referrer-Policy: no-referrer`, aucun cookie, types MIME dérivés de
  l'extension par allowlist (le reste part en téléchargement).
- **Slug non devinable** : 10 caractères Crockford (~50 bits), stable
  tant que la publication existe. `unpublish`/`delete` suppriment la ligne
  → lien mort immédiatement ; une republication ultérieure donne un
  nouveau slug.
- **Coupe-circuit opérateur** : la route vérifie l'activation du plugin
  pour l'organisation à chaque requête — désactiver le plugin coupe toutes
  les pages de l'org (le contenu reste en base, la réactivation ressuscite
  les liens).
- **Abus** : pages non indexées (`X-Robots-Tag: noindex`), non listées ;
  la purge RGPD (membre comme organisation) emporte objets et
  publications.

## Quotas

Appliqués par le service hôte, par (plugin, org, membre) :

| Limite | Défaut | Clé de configuration |
|---|---|---|
| Taille d'un objet | 16 Mio | `plugins.object_store_max_object_bytes` |
| Volume total | 64 Mio | `plugins.object_store_max_member_bytes` |
| Nombre d'objets | 500 | — |

Le plugin ajoute sa propre limite : 10 espaces par membre.

## Qualité visuelle

Le sous-agent doit charger deux compétences avant d'écrire le moindre
HTML (`internal/skills/defaults/`, ciblage `agents: [pages]`) :

- `design-web-page` — le système visuel : design tokens, typographie
  fluide (`clamp()`), palette restreinte, dark mode, finitions ;
- `responsive-mobile-first` — les règles mobiles : meta viewport, base
  360 px sans media query, cibles tactiles, jamais de défilement
  horizontal.

Corriger le style des pages produites = éditer ces compétences (admin →
Compétences), pas le prompt du plugin.

## Limites connues

- Pages **statiques** uniquement : pas de backend, pas de formulaire qui
  enregistre, pas de paiement. Le sous-agent le dit à l'utilisateur.
- `write_file` n'écrit que du texte (html, css, js…) ; les médias passent
  par `import_attachment` + `use_file`.
- L'archive zip téléchargée depuis le profil contient le **brouillon**
  (l'état le plus récent), pas la version publiée.
