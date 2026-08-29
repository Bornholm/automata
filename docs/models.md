# Catalogue de modèles

Un **client de modèle** dit à quel fournisseur s'adresser, avec quel
modèle, quelle URL et quelle clé d'API. Chaque agent, le sous-agent des
plugins et la compaction en désignent un.

Ils vivent en base et s'administrent en ligne, à l'écran « Modèles » —
catalogue, rôles de l'instance, et surcharges par organisation. Le fichier
de configuration ne décrit plus que la machine.

## Qui fait autorité

**La base, seule.** Le fichier de configuration ne déclare ni clients ni
affectations : les sections `llm_clients`, `image_clients`,
`agents.<nom>.client` et leurs semblables n'existent plus. Une instance
neuve démarre avec un catalogue vide et se règle dans l'administration —
créer les modèles, puis affecter chaque **rôle de l'instance** (écran
« Modèles »). Il n'y a aucun semis : rien ne se copie d'un fichier, rien ne
réapparaît après une suppression.

Les rôles sont dérivés de ce que le fichier DÉCLARE — un rôle par agent,
plus les fonctions actives (`plugins`, `plugins.vision`, `compaction`,
`transcription`, `consolidation`, `retrieval`, `image:<agent>`,
`embeddings:<index>`) — mais le fichier ne dit jamais QUEL modèle les sert.

Un rôle sans défaut d'instance ne casse rien en silence : l'agent répond
une erreur qui nomme le rôle, les fonctions d'arrière-plan se désactivent
avec un avertissement journalisé, et l'écran des modèles marque le rôle en
alerte.

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

En cas de panne TRANSITOIRE de lecture, l'agent réutilise le dernier
client résolu avec une trace : une base momentanément indisponible ne rend
pas l'assistant muet. Un rôle non configuré, lui, n'a pas de repli — il
doit se voir, pas se contourner.

## Différencier par organisation

L'onglet **Modèles** d'une organisation associe un client à chaque rôle :

| Rôle | Ce qu'il sert |
|---|---|
| un nom d'agent (`main`, `research`…) | l'agent lui-même |
| `plugins` | les sous-agents des plugins (atelier, agenda, pages) |
| `plugins.vision` | le regard sur les images, derrière `view_file` |
| `compaction` | la condensation de l'historique en résumé roulant |
| `image:<agent>` | le modèle qui dessine derrière `generate_image` |

Les rôles d'arrière-plan (`transcription`, `consolidation`, `retrieval`,
`embeddings:<index>`) se règlent au niveau de l'instance uniquement.

Un rôle laissé sur « défaut de l'instance » suit les rôles de l'instance
(la même table, `org_id` vide) : c'est le cas de toutes les organisations
tant que rien n'est choisi. Une organisation ne peut choisir que dans le
catalogue de l'instance ; elle n'apporte pas sa propre clé d'API.

Ce choix vit dans une table dédiée, `org_agent_clients`, et non dans
`org_settings` : la personnalisation d'une organisation ne peut que
**restreindre**, alors que choisir un modèle n'est ni une restriction ni
un octroi de capacité — c'est un routage. Même raisonnement que les
activations de plugins.

## Prise d'effet, rôle par rôle

- **À chaud, au message suivant** : les agents, `plugins`,
  `plugins.vision`, `compaction`, `image:<agent>` — résolus à chaque tour
  par le pool.
- **Au redémarrage** : `transcription`, `consolidation`, `retrieval`
  (HyDE) et `embeddings:<index>` — leurs clients sont construits au
  démarrage, hors du chemin d'un tour de conversation.
- **Verrouillé après le premier démarrage** : `embeddings:<index>`. Un
  index vectoriel est physiquement lié au modèle qui a produit ses
  vecteurs ; en changer rendrait la recherche mémoire silencieusement
  fausse. Une **sentinelle** (`<chemin-index>.embedding`) mémorise le
  modèle au premier démarrage réussi et toute divergence refuse de
  démarrer — c'est le seul refus de démarrer du catalogue, et il attrape
  toutes les voies de contournement, y compris l'édition du client dans le
  catalogue. Pour changer de modèle : supprimer l'index et sa sentinelle,
  la réindexation reconstruit.

Seuls les rôles de conversation et d'images sont surchargeables par
organisation ; les rôles d'arrière-plan sont d'instance uniquement.

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
