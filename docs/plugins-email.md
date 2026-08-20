# Plugin Email

Le plugin `email` donne à Automata un assistant de boîte mail **par
membre** : lecture et recherche IMAP, envois SMTP toujours soumis à
confirmation, et réaction aux courriels entrants.

## Configuration

1. L'administrateur active le plugin pour l'organisation (fiche de
   l'organisation → onglet Plugins).
2. Le membre configure SA boîte depuis sa page de profil (lien temporaire
   demandé à Automata : « mon profil ») : serveurs IMAP/SMTP, identifiant,
   adresse d'expédition, mot de passe. Le mot de passe est scellé au repos
   et jamais réaffiché — champ vide = inchangé.
3. Deux interrupteurs, décidés par le membre :
   - **« L'agent peut lire mes courriels »** expose `email_list_recent`,
     `email_read`, `email_search` (et permet la réaction aux entrants) ;
   - **« L'agent peut préparer des envois »** expose `email_send` et
     `email_reply`.

   Ce que l'agent ne voit pas, il ne peut pas le demander. Et quel que
   soit le réglage, **aucun courriel ne part sans un « confirmer »
   explicite** dans la conversation : l'envoi est une action de plan,
   re-autorisée au moment de la confirmation, idempotente (une
   confirmation rejouée ne renvoie pas le courriel).

## Courriels entrants

Le plugin surveille l'INBOX (relève selon `poll_seconds`, 120 s par
défaut). Un nouveau courriel déclenche un tour du sous-agent : résumé
dans la conversation privée du membre et, si une réponse semble attendue,
un brouillon proposé — à confirmer. L'événement ne transporte jamais le
corps du message : le sous-agent le lit par l'outil pendant le tour.

Bornes d'instance : `plugins.triggers.max_per_minute` par (plugin,
organisation) et `max_concurrent` global.

## Essai de bout en bout

- Compiler : `make build-plugins` (les binaires vont dans `local/plugins/`).
- Activer le plugin pour une organisation d'essai, configurer un compte
  via le profil (« Tester la connexion » vérifie l'IMAP).
- S'envoyer un courriel : le résumé arrive dans la messagerie ;
  « confirmer » expédie la réponse proposée.
- La fiche membre du plugin refuse tout sans mot de passe défini, et
  l'erreur d'authentification ne divulgue jamais le secret.
