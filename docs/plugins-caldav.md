# Plugin CalDAV

Le plugin `caldav` donne à Automata un assistant d'agenda par membre :
consultation et recherche d'un agenda CalDAV personnel, création
d'événements toujours soumise à confirmation, et, si la personne le
demande, le rangement de ses rappels dans cet agenda plutôt que dans
la base d'Automata.

Il fonctionne avec tout serveur CalDAV : Nextcloud, Fastmail, iCloud,
Radicale, Baïkal.

## Configuration

1. L'administrateur active le plugin pour l'organisation (fiche de
   l'organisation → onglet Plugins).
2. Le membre configure SON agenda depuis sa page de profil (lien
   temporaire demandé à Automata : "mon profil") : adresse du serveur,
   identifiant, mot de passe. Un mot de passe d'application vaut mieux
   que celui du compte. Le mot de passe est scellé au repos et jamais
   réaffiché. Champ vide = inchangé.
3. "Tester la connexion" vérifie les identifiants et propose la liste
   des agendas du compte. Sans choix explicite, le premier agenda publié
   par le serveur sert par défaut, ce qui convient à la majorité des
   comptes.
4. Trois interrupteurs, tous décidés par le membre :
   - "L'assistant peut consulter mon agenda" expose
     `calendar_list_events` et `calendar_search_events` ;
   - "L'assistant peut préparer des événements" expose
     `calendar_create_event` et `calendar_cancel_event` ;
   - "Ranger mes rappels dans cet agenda" confie le stockage des
     rappels au plugin (section suivante).

Comme partout ailleurs, une écriture passe de toute façon par la
confirmation humaine : ces interrupteurs décident seulement de ce que
l'assistant voit.

## Certificat auto-signé ou refusé

Un serveur auto-hébergé présente souvent un certificat qu'aucune autorité
publique n'a signé : auto-signé, émis par une autorité interne, ou
simplement expiré. La connexion échoue alors, et « Tester la connexion »
dit maintenant POURQUOI — la cause remontée par le serveur, et non une
phrase passe-partout.

Quand le motif est le certificat, la page montre ce que le serveur a
présenté : sujet, émetteur, date de fin, et l'empreinte SHA-256. Comparez
cette empreinte à celle de votre serveur avant d'accepter :

```sh
openssl s_client -connect agenda.exemple.fr:443 </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

Le bouton « Accepter ce certificat » enregistre une **exception**, au sens
du navigateur : ce certificat-là est accepté, et lui seul. Un intermédiaire
qui en présenterait un autre reste refusé — ce qu'un simple « ne pas
vérifier les certificats » accepterait sans un mot. Si le serveur renouvelle
son certificat pour un certificat valide, la connexion continue de
fonctionner ; s'il le renouvelle pour un autre certificat auto-signé, il
faut réaccepter, et c'est voulu.

L'exception appartient au membre, comme le reste de sa configuration : elle
est enregistrée pour lui seul, et le bandeau de sa page rappelle qu'elle
existe, avec de quoi la retirer.

## Les rappels rangés dans l'agenda

Quand la troisième case est cochée, `create_reminder`, `list_reminders` et
`cancel_reminder` cessent d'écrire dans la table `reminders` et travaillent
sur l'agenda. Les rappels deviennent visibles sur tous les appareils de la
personne, et déplaçables depuis n'importe quel client.

Le partage des rôles est délibéré. **Le plugin détient l'horaire et le
texte, ce qu'un agenda sait représenter. L'hôte garde la livraison :
statut, tentatives, canal de destination n'ont rien à faire dans le
calendrier de quelqu'un, et les y mettre coûterait un aller-retour réseau à
chaque tentative d'envoi.

À l'échéance, le plugin annonce l'occurrence par le flux de déclencheurs,
avec le texte à livrer. L'hôte l'envoie mot pour mot, sans tour de
modèle : un pense-bête que la personne a écrit elle-même n'a pas à être
reformulé, ni payé au prix d'un appel de LLM.

### Ce qui reste dans la base d'Automata

- Les tâches planifiées (`schedule_task`). Une tâche n'est pas un
  événement d'agenda : c'est une consigne donnée à un agent, dont le texte
  est une instruction et non quelque chose qu'on lit dans son calendrier.
- Les rappels créés avant que l'agenda ne soit branché. Ils y finissent
  leur vie et partiront normalement ; `list_reminders` les montre à côté de
  ceux de l'agenda, et `cancel_reminder` continue de les annuler. Rien
  n'est migré automatiquement.

### Ce que le plugin ne touche pas

Le magasin ne voit que les événements créés par l'assistant, marqués
par une propriété d'extension `X-AUTOMATA-REMINDER`. Une réunion, un
anniversaire, un rendez-vous médical saisis ailleurs restent invisibles de
`list_reminders` et inaccessibles à `cancel_reminder`. Ils demeurent
lisibles par les outils de consultation : c'est la suppression qui est
cloisonnée, parce qu'une suppression ne se rattrape pas.

### Récurrence

L'expression de récurrence voyage en cron à 5 champs, le dialecte de
l'hôte partout. Le plugin la traduit en `RRULE` pour que les autres clients
de l'agenda répètent l'événement correctement, et conserve l'expression
d'origine dans `X-AUTOMATA-CRON` pour un aller-retour fidèle.

Les deux langages ne se recouvrent pas, et ce qui n'est pas exprimable est
refusé, jamais approché :

- une fréquence inférieure à la journée (`*/5 * * * *`, `0 * * * *`) n'est
  pas une entrée d'agenda ;
- un jour du mois et un jour de la semaine fixés ensemble : cron
  déclenche sur l'un ou l'autre, un agenda sur les deux à la fois.
  traduire donnerait des rappels manquants ;
- les plages (`9-17`) et les pas (`*/2`) hors du champ des minutes.

Un refus remonte à la personne au lieu d'être écrit approximativement dans
son agenda.

### Chiffrement

C'est la contrepartie à peser. Automata garde le texte de ses rappels
chiffré au repos dans sa base (voir [security-model.md](security-model.md)).
Rangés dans un agenda CalDAV, ils vivent **en clair chez le fournisseur
d'agenda**, soumis à ses conditions et à ses sauvegardes. L'écran de
configuration le dit avant que la case soit cochée.

Décocher la case rend les nouveaux rappels à la base ; ceux déjà posés dans
l'agenda y restent.

### Panne du serveur d'agenda

Un serveur injoignable fait échouer la création avec un message clair.
Il n'y a délibérément aucun repli sur la base : un rappel silencieusement
éparpillé entre deux magasins est pire qu'un refus que la personne voit
passer.

## Écrire un autre magasin d'événements

Le contrat n'a rien de spécifique à CalDAV. Un plugin lève
`provides_event_store` dans son descripteur, implémente
`PutEvent`/`DeleteEvent`/`ListEvents`, et pose
`pluginsdk.EventStoreConfigKey` à `true` dans la configuration d'un membre
pour prendre la main sur ses rappels. Voir `pkg/pluginsdk/README.md`.
