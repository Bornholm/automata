# Plugin Email

Le plugin `email` donne à Automata un assistant de boîte mail **par
membre** : lecture et recherche IMAP, envois SMTP toujours soumis à
confirmation, et réaction aux courriels entrants.

## Connexion Gmail (OAuth2)

Une boîte Gmail se connecte par consentement Google, sans mot de passe
applicatif. Deux étapes, deux responsabilités :

**L'exploitant, une fois par organisation.** Dans la console Google Cloud,
créer des identifiants OAuth de type « Application Web » et déclarer
l'URI de redirection affichée par l'écran d'administration du plugin
(onglet Plugins de la fiche organisation) :

```
<web.base_url>/plugins/email/oauth/callback
```

Cette route est **publique et stable** — Google exige une URL fixe, que
les liens de profil temporaires ne peuvent pas fournir. Elle ne dessert
que le retour d'autorisation : aucun autre chemin du plugin n'est
atteignable sans session, et elle ne transporte aucune identité. C'est le
paramètre `state`, signé avec une graine propre au membre, qui désigne le
compte à connecter ; un `state` forgé ou périmé ne connecte rien.

L'identifiant et le secret client sont saisis dans ce même écran ; le
secret est scellé au repos et n'est jamais réaffiché.

**Le membre, depuis son profil.** Un bouton « Connecter Gmail », l'écran
de consentement Google, et c'est réglé : serveurs Gmail et adresse
d'expédition sont déduits, aucun mot de passe n'est conservé. Un bouton
« Déconnecter » révoque les jetons côté Automata et rend la main à la
configuration manuelle.

Automata demande un accès **hors ligne** (`access_type=offline`,
`prompt=consent`) : sans jeton durable, la boîte cesserait de fonctionner
à la première expiration — le plugin refuse donc une autorisation qui n'en
fournirait pas, plutôt que de la laisser pourrir. Le jeton d'accès est
renouvelé automatiquement quelques minutes avant son échéance ; un
consentement révoqué depuis le compte Google donne un message qui invite à
reconnecter, jamais une panne muette.

Les autres fournisseurs (IMAP/SMTP avec mot de passe applicatif) restent
disponibles dans le même écran.

## Configuration

1. L'administrateur active le plugin pour l'organisation (fiche de
   l'organisation → onglet Plugins).
2. Le membre configure SA boîte depuis sa page de profil (lien temporaire
   demandé à Automata : « mon profil ») : soit « Connecter Gmail »
   (ci-dessus), soit serveurs IMAP/SMTP, identifiant, adresse
   d'expédition et mot de passe. Le mot de passe est scellé au repos et
   jamais réaffiché — champ vide = inchangé.
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
