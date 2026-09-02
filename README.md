# Automata

Un assistant personnel qui vit dans la messagerie que vous utilisez déjà.
WhatsApp, Rocket.Chat, Discord, Signal ou le courriel. Pas une application
de plus à ouvrir.

Vous lui écrivez comme à quelqu'un. Il retient ce que vous lui confiez, vous
rappelle vos échéances, travaille sur les fichiers que vous lui envoyez, lit
vos courriels si vous le lui permettez. Et avant d'écrire quoi que ce soit
hors de la conversation, il vous demande de taper "confirmer". Toujours.
C'est la règle qui rend le reste acceptable.

Une instance peut servir plusieurs foyers ou équipes, avec facturation par
crédits, administration en ligne et export ou suppression des données de
chacun sur demande.

## Une remarque sur la langue

Le code, ses commentaires et cette documentation sont en français. Les
textes lus par les modèles de langue sont en anglais, parce que les modèles
y sont plus fiables et que personne ne les lit à part eux. Ce n'est pas un
oubli, et les contributions dans l'une ou l'autre langue sont les bienvenues.

## Ce qui compte ici

Rien ne s'exécute sans vous. Envoyer un courriel, poser un rendez-vous,
publier une page, supprimer un fichier du casier. Chacune de ces actions
devient un plan que vous confirmez en toutes lettres. Le modèle propose, il
ne décide jamais de l'identité, des permissions ni de la portée d'une
opération. J'ai vu trop d'assistants qui font confiance au modèle pour ce
genre de chose.

Vous voyez ce qu'il retient. La page de profil liste vos souvenirs mot pour
mot, tels que le modèle les relira. Vous corrigez, vous effacez. Un souvenir
faux est pire qu'un souvenir absent, alors autant pouvoir le lire.

Les plugins sont étanches. Chaque plugin tourne dans son propre processus et
ne voit que ses données. Ses écritures passent par la même confirmation. Le
SDK est sous Apache 2.0 pour que vous puissiez écrire les vôtres, ouverts ou
non.

Le reste, chiffrement au repos, cloisonnement par organisation, limites
connues, est écrit dans [docs/security-model.md](docs/security-model.md).
Lisez aussi les limites. Elles sont là exprès.

## Démarrer

Tout est dans [docs/](docs/README.md). Commencez par
[installation.md](docs/installation.md), puis la section de
[configuration.md](docs/configuration.md) qui correspond à ce que vous
voulez activer.

```sh
make build            # le binaire
make build-plugins    # les plugins de référence
make test             # la suite, avec le détecteur de course
```

Vous hébergez vous-même les services autour. Un fournisseur de modèles
compatible OpenAI (Mistral et OpenRouter marchent aussi), et pour l'atelier
de fichiers, [LeaSH](https://github.com/Bornholm/leash), un serveur qui
exécute des commandes en bac à sable sans accès réseau.

## WhatsApp, à savoir avant de brancher

Le transport WhatsApp repose sur
[whatsmeow](https://github.com/tulir/whatsmeow), qui parle le protocole de
WhatsApp Web. Ce n'est pas une API officielle. WhatsApp peut y voir une
violation de ses conditions et suspendre le compte. Ce risque est le vôtre.
Les autres transports n'ont pas ce problème.

## Licences

Automata, ce dépôt, est sous [AGPL-3.0](LICENSE). Si vous en faites tourner
une version modifiée pour d'autres, ils doivent pouvoir en obtenir les
sources.

Le SDK de plugins (`pkg/pluginsdk`) et les plugins de référence
(`plugins/*`) sont sous [Apache 2.0](pkg/pluginsdk/LICENSE). Un plugin ne se
lie qu'au SDK, jamais au reste. Vous pouvez donc écrire et distribuer les
vôtres sous la licence qui vous convient.

Pourquoi l'AGPL et pas quelque chose de plus permissif. D'abord parce que
deux dépendances sont sous GPL-3.0 (`libsignal` par whatsmeow, et
`json-repair`), ce qui ferme la porte de toute façon. Ensuite parce qu'un
logiciel qui détient vos conversations et vos souvenirs est exactement le
genre de logiciel dont on veut pouvoir lire la version qu'on utilise.

## Contribuer

Les conventions sont dans [CONTRIBUTING.md](CONTRIBUTING.md). Pour signaler
une faille, [SECURITY.md](SECURITY.md).
