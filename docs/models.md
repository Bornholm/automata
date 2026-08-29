# Catalogue de modèles

Un **client de modèle** dit à quel fournisseur s'adresser, avec quel
modèle, quelle URL et quelle clé d'API. Chaque agent, le sous-agent des
plugins et la compaction en désignent un.

Ces clients vivaient dans le fichier de configuration. Depuis la migration
0022, **la base de données fait autorité** : ils s'éditent dans
l'administration, à l'écran « Modèles », et une organisation peut se voir
servir un modèle différent des autres.

## Qui fait autorité

Au tout premier démarrage d'une instance, les sections `llm_clients` et
`image_clients` du fichier YAML sont **copiées en base**. Un jalon
(`llm-clients-seed` dans `maintenance_runs`) marque ce semis comme fait :
il n'est jamais rejoué, même si le catalogue est ensuite vidé — ce que
l'opérateur a supprimé ne doit pas réapparaître au redémarrage suivant.

Passé ce semis, **le contenu de ces sections n'est plus lu**. Modifier un
modèle dans le YAML n'a plus d'effet ; c'est l'administration qui compte.

La section reste néanmoins **requise et validée** : c'est elle qui dit quel
client sert quel rôle par défaut. `agents.main.client: principal` désigne
l'entrée `principal` du catalogue, et ce câblage-là n'a pas migré.

## Prise d'effet immédiate

Une modification s'applique **au message suivant, sans redémarrage**.

Les agents ne capturent plus un client figé au démarrage : à chaque tour,
ils demandent au pool celui de leur rôle. Le pool relit la définition en
base — une lecture par clé primaire, quelques microsecondes — et ne
reconstruit le client que si elle a changé. Il n'y a donc aucune
invalidation de cache à gérer, et le résultat reste juste même quand un
autre processus écrit en base.

Reconstruire un client réinitialise son disjoncteur : une configuration
corrigée repart tout de suite, sans attendre la fin d'une fenêtre d'échec.

Le client construit au démarrage depuis la configuration reste le **repli** :
si le catalogue est illisible, si le client d'une organisation a été
supprimé, ou si le fournisseur n'est pas reconnu, l'agent l'utilise et
journalise l'incident. Une base en panne ne rend jamais l'assistant muet.

## Différencier par organisation

L'onglet **Modèles** d'une organisation associe un client à chaque rôle :

| Rôle | Ce qu'il sert |
|---|---|
| un nom d'agent (`main`, `research`…) | l'agent lui-même |
| `plugins` | les sous-agents des plugins (atelier, agenda, pages) |
| `plugins.vision` | le regard sur les images, derrière `view_file` |
| `compaction` | la condensation de l'historique en résumé roulant |
| `image:<agent>` | le modèle qui dessine derrière `generate_image` |

Un rôle laissé sur « défaut de l'instance » suit la configuration : c'est
le cas de toutes les organisations tant que rien n'est choisi. Une
organisation ne peut choisir que dans le catalogue de l'instance ; elle
n'apporte pas sa propre clé d'API.

Ce choix vit dans une table dédiée, `org_agent_clients`, et non dans
`org_settings` : la personnalisation d'une organisation ne peut que
**restreindre**, alors que choisir un modèle n'est ni une restriction ni
un octroi de capacité — c'est un routage. Même raisonnement que les
activations de plugins.

## Ce qui ne bouge pas

Les clients de la **mémoire** restent dans le YAML, construits au
démarrage, et ne sont ni éditables à chaud ni surchargeables :

- `memory.indexes[].client` (**embeddings**) — un index vectoriel est
  construit avec un modèle donné ; en changer rendrait les vecteurs déjà
  écrits incomparables aux nouveaux, et la recherche mémoire se
  dégraderait en silence, sans erreur ;
- `memory.retrieval.client` (HyDE) et `memory.consolidation.client` — les
  faire varier par organisation n'aurait pas de sens.

Trois autres usages s'appuient sur des clients du catalogue — on peut donc
les y modifier — mais construisent le leur **au démarrage** : la
modification n'y prend effet qu'après un redémarrage du service, et ils
n'apparaissent pas parmi les rôles réglables par organisation.

- `audio.transcription_client` — la transcription des notes vocales ;
- `memory.retrieval.client` (HyDE) et `memory.consolidation.client`.

C'est une limite de cette version, pas une décision : ces composants
construisent leur client hors du chemin d'un tour de conversation, et les
brancher sur le pool demande un travail distinct de celui livré ici.

La définition des **agents** eux-mêmes — prompts, délégations, serveurs
MCP, limites — reste entièrement dans le YAML. Seul le client qu'ils
utilisent est administrable.

## Les clés d'API

La clé est **scellée au repos** (AES-256-GCM), avec une clé dérivée du
secret de session par un contexte qui lui est propre
(`automata/llm_clients/v1`) : un secret de plugin ou de plateforme ne peut
jamais ouvrir une clé de fournisseur, ni l'inverse.

Elle n'est **jamais relue par l'interface**. L'écran dit seulement si une
clé est enregistrée, et ne propose que de la remplacer — un champ laissé
vide la conserve. Seul le pool l'ouvre, au moment de construire le client.
En cas d'oubli, il faut la ressaisir.

Comme pour les autres secrets scellés : **changer `web.session_secret`
rend les clés illisibles** et impose de toutes les ressaisir.

## Supprimer un modèle

La suppression est refusée tant qu'un rôle de la configuration ou une
organisation s'y réfère — l'écran dit qui le retient. Sans quoi la
suppression laisserait des agents sans modèle.

## Organisations gratuites sans limite

Sans rapport direct avec le catalogue, mais réglé au même endroit : l'onglet
**Crédits** d'une organisation porte une bascule « Gratuite, sans limite ».

Elle se distingue de « offerte par la maison », qui accorde une allocation
mensuelle plafonnée — laquelle finit par s'épuiser et met le service en
pause. Une organisation sans limite n'est **jamais débitée, jamais mise en
pause, jamais alertée** sur son solde.

Sa consommation reste **mesurée** (`usage_records`) : son coût réel
demeure visible dans les écrans de consommation et de marge. C'est ce qui
distingue « offert » de « non compté » — vous savez toujours ce que cette
organisation vous coûte.

Côté membre, le profil affiche l'usage du mois à titre indicatif, sans
solde ni bouton d'achat.
