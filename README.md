# Automata

Un assistant personnel qui vit dans votre messagerie — WhatsApp, Rocket.Chat,
Discord, Signal, courriel — plutôt que dans une application de plus.

Automata reçoit vos messages, les confie à un agent généraliste qui délègue à
des spécialistes, tient une mémoire persistante cloisonnée par personne et par
groupe, programme des rappels et des tâches, manipule vos fichiers dans un bac
à sable, et **demande toujours confirmation, en toutes lettres, avant toute
action qui écrit quelque part**. Il peut servir plusieurs organisations sur
une même instance, avec facturation, administration en ligne et export ou
suppression des données personnelles à la demande.

> **Langue.** Le code, ses commentaires et la documentation sont écrits en
> français. Les textes destinés aux modèles de langue sont en anglais. C'est
> un choix délibéré, pas un oubli — les contributions sont bienvenues dans
> l'une ou l'autre langue.

## Ce qui le distingue

- **Rien ne s'exécute sans vous.** Toute écriture externe — envoyer un
  courriel, poser un rendez-vous, publier une page — passe par un plan
  d'actions que vous confirmez par le mot « confirmer ». Le modèle ne décide
  jamais de l'identité, des permissions ni de la portée d'une opération.
- **Une mémoire que vous voyez.** Ce qu'Automata retient de vous se lit mot
  pour mot dans votre profil, se corrige et s'efface.
- **Un système de plugins étanche.** Chaque plugin est un sous-processus qui
  ne voit que ses propres données ; ses écritures passent par la même
  confirmation humaine. Le SDK de plugins est sous licence Apache 2.0 pour
  que chacun écrive les siens, ouverts ou non.
- **Chiffré au repos**, cloisonné par organisation, et documenté jusque dans
  ses limites : voir [docs/security-model.md](docs/security-model.md).

## Démarrer

La documentation complète est dans [`docs/`](docs/README.md). Pour une
première installation : [docs/installation.md](docs/installation.md), puis
la section de [docs/configuration.md](docs/configuration.md) correspondant à
ce que vous voulez activer.

```sh
make build            # compile le binaire
make build-plugins    # compile les plugins de référence
make test             # lance la suite de tests
```

Le binaire s'appuie sur des services séparés que vous hébergez : un
fournisseur de modèles compatible OpenAI (ou Mistral, OpenRouter…), et pour
l'atelier de fichiers, [LeaSH](https://github.com/Bornholm/leash), un serveur
d'exécution en bac à sable.

## Avertissement sur WhatsApp

Le transport WhatsApp repose sur [whatsmeow](https://github.com/tulir/whatsmeow),
une implémentation du protocole WhatsApp Web, et non sur une API officielle.
Son usage peut contrevenir aux conditions d'utilisation de WhatsApp ; un
compte peut être suspendu. Vous seul portez ce risque. Les autres transports
(Rocket.Chat, Discord, Signal, courriel) n'ont pas cette réserve.

## Licences

- **Automata** (ce dépôt) : [GNU Affero General Public License v3.0](LICENSE).
  Si vous en faites tourner une version modifiée pour des tiers, ils doivent
  pouvoir en obtenir les sources.
- **SDK de plugins** (`pkg/pluginsdk`) et **plugins de référence**
  (`plugins/*`) : [Apache License 2.0](pkg/pluginsdk/LICENSE). Un plugin ne
  se lie qu'au SDK, jamais à Automata : vous pouvez écrire et distribuer les
  vôtres sous la licence de votre choix.

Pourquoi l'AGPL : Automata embarque des dépendances sous GPL-3.0
(`libsignal` via whatsmeow, `json-repair`), ce qui exclut une licence
permissive pour l'ensemble ; et un assistant qui détient les conversations et
les souvenirs de ses utilisateurs est précisément le type de logiciel dont on
veut pouvoir lire la version que l'on utilise.

## Contribuer

Voir [CONTRIBUTING.md](CONTRIBUTING.md) pour les conventions du projet, et
[SECURITY.md](SECURITY.md) pour signaler une faille.
