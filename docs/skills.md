# Compétences (skills)

Une **compétence** est un mode opératoire en markdown : la bonne façon de
faire une chose précise, écrite une fois, que les agents chargent au
moment où ils en ont besoin.

Elle répond à un problème observé en production : devant une tâche qu'il
n'a jamais faite, un sous-agent improvise sa méthode, essaie, se trompe,
et épuise son budget d'outils sans rien produire. Mettre la recette dans
le prompt système réglerait le cas — mais ferait payer chaque recette à
chaque tour, pour tous les messages, y compris ceux qui n'en ont rien à
faire.

## Divulgation progressive

Le compromis retenu est celui des Agent Skills d'Anthropic :

1. l'agent voit en permanence un **catalogue** — une ligne par
   compétence, nom et description, quelques dizaines de tokens ;
2. quand une compétence correspond à sa tâche, il appelle l'outil
   **`load_skill`** avec son nom ;
3. il reçoit alors le **contenu complet** et le suit.

Charger une compétence ne coûte ni appel au modèle ni requête réseau :
c'est une lecture en base, rendue telle quelle.

Le catalogue et l'outil sont montés **à chaque tour**. Une compétence
ajoutée, modifiée ou désactivée dans l'administration s'applique au
message suivant, sans redémarrage du service.

## Qui voit quoi

Le ciblage se déclare par agent :

- ciblage vide : la compétence est visible de **tous** les agents ;
- `agents: [workspace]` : visible du seul sous-agent du plugin
  `workspace`.

Le nom d'agent est celui de la configuration (`agents.<nom>`) ou, pour un
sous-agent de plugin, le nom du plugin.

Le ciblage n'est pas une décoration : `load_skill` le revérifie. Un nom
deviné par le modèle pour un agent non ciblé reste introuvable, et une
compétence désactivée l'est aussi.

Les agents équipés sont l'orchestrateur, les spécialistes MCP et les
sous-agents de plugins. Le spécialiste de vision et le générateur
d'images ne le sont pas : ils n'ont pas de boucle d'outils à nourrir.

## Le format

Un fichier de compétence est un frontmatter YAML suivi d'un corps
markdown :

```markdown
---
name: mask-logo-in-image
description: Remove or mask a logo or watermark on a photo with imagemagick
agents: [workspace]
---

# Mask a logo or watermark on an image

1. Locate the logo with view_file…
```

| Champ         | Obligatoire | Rôle                                                    |
| ------------- | ----------- | ------------------------------------------------------- |
| `name`        | oui         | Clé de la compétence, en kebab-case (`^[a-z0-9][a-z0-9-]*$`) |
| `description` | oui         | La ligne du catalogue : une phrase, en anglais          |
| `agents`      | non         | Noms des agents ciblés ; absent = tous                  |

**Le contenu part au modèle : il s'écrit en ANGLAIS.** C'est la règle du
dépôt pour tout ce qui traverse le prompt (voir `AGENTS.md`). Le reste —
code, journaux, cette documentation — reste en français.

Une bonne compétence est une recette, pas un essai : les étapes dans
l'ordre, les commandes exactes, et ce qu'il ne faut PAS faire. Une
quarantaine de lignes suffisent.

## Le semis au démarrage

Les compétences fournies par le projet vivent dans
`internal/skills/defaults/` et sont embarquées dans le binaire
(`go:embed`). À chaque démarrage, elles sont insérées en base **si et
seulement si leur nom est absent**.

Une compétence fournie et **jamais modifiée** suit ensuite les mises à jour
du dépôt : corriger une recette livrée profite aux instances déjà semées,
sans intervention. Dès qu'un administrateur l'édite, elle est figée — son
travail prime sur le contenu embarqué, et un redéploiement ne l'écrase
**jamais**. Le bouton « Restaurer la version d'origine » lève ce gel et
remet la version du dépôt.

Ce statut repose sur une colonne `edited` explicite, pas sur une
comparaison de dates : les horodatages perdent en précision au stockage, et
une édition faite dans la même seconde que le semis serait indiscernable
d'une absence d'édition.

Le journal du démarrage rend compte du semis en compteurs seulement :

```
skills: semées count=3 inserted=3
```

Supprimer une compétence fournie par le projet la fait revenir au
prochain démarrage : pour la neutraliser durablement, désactivez-la
plutôt.

## L'administration

Écran **Compétences** de l'administration web :

- liste : nom, description, agents ciblés, état, date de modification ;
- création : nom (définitif), description, ciblage, contenu markdown ;
- édition : tout sauf le nom — c'est la clé, renommer revient à créer
  puis supprimer ;
- activation/désactivation par case à cocher ;
- suppression, avec confirmation ;
- restauration, sur les seules compétences fournies par le projet.

La bibliothèque est celle de l'**instance** : elle n'a pas encore de
dimension par organisation. Elle en prendra une le jour où le besoin
apparaîtra, sur le modèle de `plugin_activations`.

## Ajouter une compétence au dépôt

1. Écrire `internal/skills/defaults/<nom>.md`, frontmatter compris, en
   anglais.
2. `go test ./internal/skills/` — le test du paquet refuse un frontmatter
   incomplet ou un nom hors kebab-case.
3. Déployer. Le semis l'insère au démarrage ; les instances qui portent
   déjà ce nom ne sont pas touchées.

Rien à changer dans `config.yaml` : le système est actif dès que la table
existe. Une instance sans compétence active a simplement un catalogue
vide, et aucun outil `load_skill` monté.

## Les compétences fournies avec le projet

| Compétence | Pour qui | Ce qu'elle règle |
| --- | --- | --- |
| `mask-logo-in-image` | workspace | Masquer un logo sur une photo en une commande, livrer avant de vérifier. |
| `remove-video-watermark` | workspace | Retirer un filigrane d'une vidéo (`delogo`), en surveillant la taille de sortie. |
| `edit-office-document` | workspace | Lire, modifier et convertir un docx/odt/pdf, en avertissant des pertes de mise en page. |
| `compress-media-for-messaging` | workspace | Ramener une vidéo ou une photo sous la limite d'envoi. |
| `scan-to-pdf` | workspace | Transformer des photos de documents en PDF redressé, lisible et **cherchable** (OCR). |
| `strip-photo-metadata` | workspace | Lire ou effacer les métadonnées d'une photo (position GPS, appareil) avant partage. |
| `unslop` | tous | Débarrasser un texte long de ses tics d'écriture d'IA. |
| `translate` | tous | Traduire en gardant registre, noms propres et mise en forme. |

Les deux dernières ne sont pas ciblées : elles apparaissent dans le
catalogue de tous les agents équipés. `unslop` vise les textes destinés à
être lus par d'autres — un courriel, un document — et non les réponses de
conversation, dont le ton est déjà cadré par le prompt de l'agent.
