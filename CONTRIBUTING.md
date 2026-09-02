# Contribuer

Merci de votre intérêt. Ce document dit ce qui rend une contribution facile
à relire et à intégrer.

## Langue

- **Code, commentaires, documentation, messages de commit : en français.**
- **Tout texte destiné à un modèle de langue** (prompts système, descriptions
  d'outils, consignes de sous-agents) : **en anglais**. Les modèles y sont
  plus fiables, et ces textes ne sont jamais montrés aux utilisateurs.
- Textes vus par les utilisateurs dans la conversation ou l'interface : en
  français.

Les issues et discussions sont bienvenues dans les deux langues.

## Commits

Une ligne, au présent, préfixée du type : `feat:`, `fix:`, `refactor:`,
`docs:`, `test:`, `chore:`. Pas de corps, pas de pied de page.

```
fix: let members cancel the reminders and tasks they scheduled
```

Un commit = un changement qui compile et passe les tests. Une fonctionnalité
en plusieurs étapes fait plusieurs commits, chacun vert.

## Avant d'ouvrir une pull request

```sh
make fmt        # gofmt + templ
make vet
make test
make build-plugins
make web-generate   # si vous avez touché un .templ
```

La CI rejoue la même chose.

## Ce qui ne se négocie pas

Ces invariants sont la raison d'être du projet. Une contribution qui en
contourne un sera refusée, quelle que soit sa qualité par ailleurs.

1. **Aucune écriture externe sans confirmation humaine littérale.** Tout outil
   de plugin qui n'est pas `read_only` passe par un plan d'actions confirmé
   par le mot « confirmer ». Le modèle ne décide jamais de l'identité, de la
   portée ni des permissions d'une opération.
2. **Le bac à sable d'exécution reste sans réseau** (`policies/toolbox.yaml`
   dans LeaSH). C'est ce qui rend acceptable d'y exécuter des commandes sans
   confirmation. N'y ajoutez pas de binaire réseau.
3. **Aucun contenu privé dans les journaux** : identifiants et compteurs
   seulement, jamais un message, une transcription ni une pièce jointe.
4. **Les secrets sont scellés au repos** et jamais relus par une interface :
   on remplace une clé, on ne l'affiche pas.
5. **Le cloisonnement par portée s'applique au niveau du stockage**, pas
   seulement de l'affichage.

## Style

- Un commentaire explique un *pourquoi* — une décision, un piège rencontré,
  une contrainte extérieure. Le *quoi* est dans le code.
- Une constante nommée plutôt qu'un nombre en dur, avec la raison de sa
  valeur.
- Une erreur remontée à l'utilisateur dit quoi faire ensuite.
- Un test porte le nom du comportement qu'il garantit, et son commentaire dit
  quel incident ou quel piège il empêche de revenir.

## Écrire un plugin

Le SDK (`pkg/pluginsdk`) est sous Apache 2.0 : votre plugin peut avoir la
licence de votre choix. Les plugins de référence dans `plugins/` sont des
points de départ que vous pouvez copier. Voir `docs/plugins-*.md`.
