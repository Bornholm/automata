# Signaler une faille

Automata détient les conversations, les souvenirs et parfois les courriels de
ses utilisateurs : une faille y est grave. Merci de la signaler **en privé**
plutôt que par une issue publique.

## Comment

Écrivez à l'adresse indiquée sur le profil GitHub du mainteneur
([@Bornholm](https://github.com/Bornholm)), ou utilisez le signalement privé
de GitHub (« Report a vulnerability » dans l'onglet Security du dépôt) s'il
est activé.

Décrivez ce qu'il faut pour reproduire : version ou commit, configuration
pertinente (sans vos secrets), étapes, effet observé. Un correctif est
d'autant plus rapide que la reproduction est nette.

## Ce à quoi vous attendre

- Un accusé de réception sous une semaine.
- Une évaluation, puis un correctif si la faille est confirmée, publié avec
  une note dans l'historique des versions. Vous serez crédité si vous le
  souhaitez.
- Pas de programme de récompense : c'est un projet personnel.

## Périmètre

Le modèle de menace, les frontières de confiance et les **limitations
assumées** sont écrits dans [docs/security-model.md](docs/security-model.md).
Lisez-le avant de signaler : ce qui y figure comme limitation connue n'est
pas une faille — un rapport qui la conteste avec un scénario nouveau, en
revanche, est bienvenu.

Points d'attention particuliers, où un rapport a le plus de valeur :

- toute façon de faire exécuter une action externe **sans** confirmation
  humaine littérale ;
- toute lecture ou écriture qui traverse une frontière de portée (une
  personne, un groupe, une organisation, un plugin) ;
- toute évasion du bac à sable d'exécution des fichiers (LeaSH) ;
- toute fuite de contenu privé dans les journaux, les métriques ou les
  messages destinés au modèle.
