# AGENTS.md

Règles d'implémentation pour ce dépôt. Voir `PLAN.md` pour le plan complet.

## Travail séquentiel

- Travailler sur une seule phase à la fois.
- Avant chaque phase, inspecter les API réelles concernées (ne pas supposer
  qu'une méthode existe à partir du plan).
- Maintenir le dépôt compilable et testé après chaque étape.

## Pendant l'implémentation

- Changements chirurgicaux, pas d'abstractions spéculatives.
- Vocabulaire francophone du dépôt : employer exclusivement `org`, jamais
  `family`.
- Ne pas déplacer les règles de sécurité dans les prompts.
- Ne pas exposer les identifiants de ressources externes aux modèles.
- Ne pas stocker les audios ou transcriptions.
- Les pièces jointes non vocales (images, documents) sont conservées, elles :
  voir `internal/media` et la table `message_attachments`. Ne jamais
  journaliser leurs octets, ni les transmettre au modèle sans passer par le
  filtre de `config.Attachments` — un fournisseur refuse la requête ENTIÈRE
  lorsqu'une pièce jointe ne lui convient pas.
- Ne jamais attacher de média à un message `system` ou `assistant` : les
  fournisseurs les rejettent. Seuls les messages `user` et les résultats
  d'outils peuvent en porter.
- Ne pas journaliser les contenus privés.
- Toujours consommer les canaux de streaming jusqu'à fermeture ou annulation.
- Toujours fermer les lecteurs, connexions et goroutines.
- Utiliser `context.Context` pour les opérations bloquantes.
- Envelopper les erreurs avec un contexte utile ; ne pas ignorer une erreur
  sans justification explicite.

## Après chaque phase

```bash
make fmt
make test
make vet
make build
```

Exécuter aussi `make race` lorsque pertinent.

## Commits

- Un message par ligne, en anglais, format `type: description` (ex.
  `feat: initial commit`).
- Jamais de corps de message, jamais de trailer `Co-Authored-By`.
