package main

// configTemplate produit le fichier de configuration.
//
// Un gabarit texte plutôt qu'une sérialisation de config.Config : la
// sérialisation perdrait les commentaires, émettrait les champs laissés vides
// et imposerait son propre ordre. Ici, le fichier généré ressemble à celui
// qu'un humain aurait écrit, commentaires compris.
//
// Les valeurs interpolées viennent du wizard, jamais d'une saisie brute
// dangereuse : identifiants courts, chemins, noms de variables normalisés par
// envVarName.
const configTemplate = `# Configuration Automata générée par « automata config init ».
#
# Aucun secret n'est écrit ici : les valeurs sensibles sont référencées par
# variable d'environnement et lues au chargement. La liste complète des
# variables à définir est dans le fichier d'environnement généré à côté.
#
# Attention : l'expansion des variables s'applique au fichier ENTIER, y
# compris aux commentaires. N'écrivez pas de référence d'environnement dans un
# commentaire, même à titre d'exemple.
#
# Référence complète de chaque section : docs/configuration.md

version: 1

# Forme abrégée, suffisante pour une organisation unique. Pour en servir
# plusieurs sur la même instance, voir docs/configuration.md#organizations.
organization:
  id: {{ .OrgID }}
  display_name: {{ .OrgDisplayName }}

storage:
  # Chiffrement au repos des contenus personnels : messages, résumés de
  # conversation, rappels et pièces jointes. La clé se sauvegarde À PART :
  # la perdre rend ces contenus définitivement illisibles, sauvegardes
  # comprises. Retirer cette ligne écrit tout en clair.
  encryption_key: ${STORAGE_ENCRYPTION_KEY}
  application:
    driver: sqlite
    path: {{ .DataDir }}/app.sqlite
    pragmas:
      foreign_keys: true
      journal_mode: WAL
      busy_timeout: 5s

# Les comptes de messagerie (WhatsApp, Signal…) ne se déclarent pas ici :
# ils se créent dans l'administration (Canaux et plateformes), qui range
# leur configuration chiffrée en base.
{{ if .Web }}
web:
  enabled: true
  addr: {{ .WebAddr }}
  # URL publique de l'instance : elle compose les liens de profil envoyés
  # dans les conversations, et sert d'adresse de retour aux paiements
  # comme aux autorisations OAuth des plugins. Elle doit être joignable
  # depuis l'extérieur, en HTTPS hors développement local.
  base_url: ${WEB_BASE_URL}
  session_secret: ${WEB_SESSION_SECRET}
  admin:
    email: ${WEB_ADMIN_EMAIL}
    # Empreinte bcrypt, produite par « automata web hash-password ».
    password_hash: ${WEB_ADMIN_PASSWORD_HASH}
{{ end }}{{ if .Plugins }}
plugins:
  enabled: true
  dir: {{ .PluginsDir }}
  # Les modèles des sous-agents et de view_file se règlent en ligne :
  # rôles « plugins » et « plugins.vision » de l'écran des modèles.
{{ end }}{{ if .Observability }}
observability:
  enabled: true
  addr: {{ .ObservabilityAt }}
{{ end }}{{ if .Audio }}
audio:
  enabled: true
  # Le modèle de transcription se règle en ligne : rôle « transcription ».
  max_size: 20MiB
  timeout: 2m
  # Ni l'audio ni sa transcription ne sont conservés.
  persist_audio: false
  persist_transcription: false
{{ end }}{{ if .Attachments }}
attachments:
  enabled: true
  max_size: 8MiB
  max_count: 4
  # Un fournisseur refuse la requête ENTIÈRE si une pièce jointe ne lui
  # convient pas. Cette liste doit rester alignée sur ce que le modèle
  # configuré accepte réellement.
  accepted_types:
    - image/png
    - image/jpeg
    - image/webp
    - image/gif
    - text/plain
  # Pièces jointes rejouées depuis l'historique à chaque tour. Chacune est
  # retransmise au modèle à chaque message suivant : cette valeur pèse
  # directement sur le coût.
  max_history: 4
  max_reply: 3
{{ end }}
# Historique conversationnel et sa compaction.
conversation:
  # Messages passés rejoués au modèle à chaque tour. Au-delà, sans
  # compaction, ils sortent simplement du contexte : l'assistant perd le
  # fil d'une conversation suivie.
  history_limit: 20
  compaction:
    enabled: true
    # Le modèle se règle en ligne (rôle « compaction ») ; un modèle
    # économique suffit, et l'échec d'une compaction n'est jamais bloquant.
    max_summary_chars: 2000
    # Les faits durables (préférences, décisions, engagements, dates)
    # rejoignent la mémoire à long terme, dans la portée de la
    # conversation. Avec memory.consolidation, cela forme le cycle
    # complet : les faits entrent au fil des compactions, la nuit les
    # fusionne et purge les périmés.
    extract_facts: true
    max_facts: 5
    # record_episodes conserve le fragment condensé VERBATIM dans le store
    # mémoire. Son contenu y est chiffré au repos avec la même clé que la
    # base applicative ; seuls les métadonnées et le dictionnaire de termes
    # de l'index de recherche restent en clair — chiffrer des termes
    # indexés casserait la recherche. Laissé désactivé faute d'usage : les
    # épisodes ne servent qu'avec le drapeau memory.history de l'agent.
    record_episodes: false

# Les modèles (fournisseur, modèle, clé d'API) ne se déclarent pas ici :
# le catalogue vit en base et s'administre en ligne (/admin/llm-clients),
# tout comme l'affectation d'un modèle à chaque agent et à chaque fonction
# (« rôles de l'instance »). Une instance neuve se règle depuis
# l'administration au premier démarrage. Voir docs/models.md.

agents:
  main:
    type: orchestrator
    system_prompt:
      file: {{ .PromptsDir }}/main.md
{{- if .Web }}
    # Ouvre à l'interlocuteur un lien temporaire vers sa page de profil
    # (crédits, paiement, données personnelles). Sans lui, l'assistant
    # n'a aucun moyen de donner son propre lien à quelqu'un qui le
    # demande dans la conversation.
    profile_link: true
{{- end }}
    delegates:
      - vision
      - imagine
{{- range .Agents }}
      - {{ .Name }}
{{- end }}
    # Rappels ponctuels : délivrer un texte à l'échéance.
    reminders: true
    # Tâches planifiées : faire TRAVAILLER l'agent à l'échéance. Pouvoir
    # distinct du rappel, et bridé — pendant un tour planifié, les actions
    # sensibles proposées sont ignorées, jamais exécutées sans humain
    # devant l'écran.
    scheduled_tasks: true
    memory:
      search: true
      remember: true
      forget: true
      # Retrouver ce qui a été dit plus tôt dans la même conversation,
      # verbatim et daté, au-delà de l'historique récent.
      history: true
      # Rappel automatique : à chaque tour, les souvenirs pertinents sont
      # cherchés sur le message entrant et injectés dans le contexte. Sans
      # lui, la mémoire n'est lue que si le modèle pense à appeler
      # search_memory — ce qu'il ne fait pas pour une préférence qu'il ne
      # sait pas qu'il ignore. C'est ce drapeau qui fait la différence
      # entre un assistant qui se souvient et un qui stocke.
      recall: true
    limits:
      max_sequential_tool_calls: 8
      max_actions_per_turn: 10
      tool_timeout: 30s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 64KiB
  # Regarde les images et documents joints, et rapporte ce qu'ils
  # contiennent. C'est ce qui permet à un généraliste texte-seul de
  # répondre à « qu'est-ce qu'il y a sur cette photo ? ».
  vision:
    type: specialist
    description: looks at the images and documents attached to the conversation and reports what they contain
    system_prompt:
      file: {{ .PromptsDir }}/vision.md
    # Sans image, ce spécialiste n'a rien à examiner : l'orchestrateur ne le
    # sollicite pas. Sollicité à vide, un modèle multimodal décrit une image
    # qui n'existe pas au lieu de constater qu'il ne voit rien.
    requires_attachments: true
    limits:
      max_sequential_tool_calls: 1
      max_actions_per_turn: 1
      tool_timeout: 30s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 32KiB

  imagine:
    type: specialist
    description: generates images from text descriptions
    system_prompt:
      file: {{ .PromptsDir }}/imagine.md
    # Le modèle qui dessine se règle en ligne : rôle « image:imagine ».
    image_generation: true
    limits:
      max_sequential_tool_calls: 3
      max_actions_per_turn: 2
      # La génération d'une image dépasse largement une complétion.
      tool_timeout: 60s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 32KiB
{{ range .Agents }}
  {{ .Name }}:
    type: specialist
{{- if .Description }}
    # Repris dans l'outil delegate_to_{{ .Name }} exposé au généraliste :
    # c'est là-dessus qu'il décide de déléguer.
    description: {{ .Description }}
{{- end }}
    system_prompt:
      file: {{ .PromptFile }}
    mcp_servers:
      - {{ .Server }}
    limits:
      max_sequential_tool_calls: 6
      max_actions_per_turn: 5
      tool_timeout: 30s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 32KiB
{{ end }}
{{- if .Servers }}
# Tout le comportement applicatif d'un service se déclare ici : ressource à
# injecter, outils exigeant confirmation, domaine de permission. L'application
# ne connaît aucun domaine par son nom.
mcp_servers:
{{- range .Servers }}
  {{ .Name }}:
    transport: {{ .Transport }}
    url: ${{"{"}}{{ .URLVar }}{{"}"}}
{{- if .TokenVar }}
    headers:
      Authorization: Bearer ${{"{"}}{{ .TokenVar }}{{"}"}}
{{- end }}
{{- if .ResourceKey }}
    # Identifiant injecté par l'application dans chaque appel, lu dans
    # channels[].resources.{{ .ResourceKey }} selon la portée courante.
    resource:
      key: {{ .ResourceKey }}
      parameter: {{ .ResourceParam }}
    permission_domain: {{ .PermissionDomain }}
    tools:
      # Les outils d'écriture deviennent des actions à confirmer.
      confirm_writes: true
      read_prefixes: [list_, get_, search_, find_]
{{- if .RequireRFC3339 }}
      require_rfc3339: [start, end, start_time, end_time]
{{- end }}
{{- if .DedupeWrites }}
      dedupe_writes: true
{{- end }}
{{- else }}
    # Aucune ressource, aucune confirmation : tous les outils s'exécutent
    # directement, réglage d'un service en lecture seule.
{{- end }}
{{- end }}
{{- end }}

memory:
  store:
    driver: sqlite
    path: {{ .DataDir }}/amoxtli.sqlite
  indexes:
    - id: lexical
      type: bleve
      path: {{ .DataDir }}/memory.bleve
      weight: 1
  policies:
    private_can_write_org: false
    org_readable_by_children: true
  # Réorganisation nocturne : fusion des redondances et oubli des faits
  # périmés, portée par portée. Sans elle, la mémoire s'accumule sans
  # limite et la recherche se dégrade à mesure qu'elle grossit.
  consolidation:
    enabled: true
    # Le modèle se règle en ligne : rôle « consolidation ».
    # 4h40, heure locale du serveur.
    cron: "40 4 * * *"
    # En dessous de ce nombre de souvenirs, une portée est laissée intacte :
    # rien à gagner, et aucun appel au modèle dépensé.
    min_memories: 10
    # Réflexion épisodique : la même passe relit les épisodes verbatim
    # récents pour en dégager des habitudes ou préférences récurrentes
    # jamais énoncées explicitement. Coûteuse en tokens et spéculative :
    # désactivée par défaut.
    reflection:
      enabled: false
      min_episodes: 5
      # Âge (en jours) au-delà duquel un épisode déjà réfléchi est purgé.
      # 0 : conservation illimitée.
      retention_days: 0

identities:
  roles:
    adult:
      permissions:
        - memory.personal.read
        - memory.personal.write
        - memory.personal.delete
        - memory.group.read
        - memory.group.write
        - memory.group.delete
        - memory.org.read
        - memory.org.write
        - memory.org.delete
        - calendar.personal.read
        - calendar.personal.write
        - calendar.group.read
        - calendar.group.write
        - calendar.org.read
        - todo.personal.read
        - todo.personal.write
        - todo.group.read
        - todo.group.write
        - reminder.personal.read
        - reminder.personal.write
        - reminder.group.read
        - reminder.group.write
        - task.personal.read
        - task.personal.write
        - task.group.read
        - task.group.write
    child:
      permissions:
        - memory.personal.read
        - memory.personal.write
        - memory.group.read
        - memory.org.read
        - calendar.personal.read
        - calendar.group.read
        - calendar.org.read
        # Se poser un rappel est anodin ; planifier un travail récurrent de
        # l'assistant est un autre pouvoir, réservé aux adultes.
        - reminder.personal.read
        - reminder.personal.write
        - todo.personal.read
        - todo.group.read
{{- if .Schedule }}
    # Principal technique de la tâche planifiée : lecture seule, jamais les
    # permissions d'un administrateur.
    scheduled-reader:
      permissions:
        - memory.org.read
        - calendar.org.read
        - todo.org.read
{{- end }}

  principals:
{{- range .Principals }}
    - id: {{ .ID }}
      kind: human
      display_name: {{ .DisplayName }}
      roles: [{{ .Role }}]
{{- if .MCP }}
      # Connexions MCP propres à cette personne. Chacune obtient une session
      # isolée : deux personnes d'un même groupe ne partagent jamais un jeton.
      mcp:
{{- range .MCP }}
        {{ .Server }}:
          headers:
            Authorization: Bearer ${{"{"}}{{ .TokenVar }}{{"}"}}
{{- end }}
{{- end }}
{{- end }}
{{- if .Schedule }}
    - id: scheduler-readonly
      kind: service
      display_name: Planificateur
      roles: [scheduled-reader]
{{- end }}

# Un message dont l'origine n'est pas déclarée ici est ignoré sans le moindre
# appel au modèle.
origins:
{{- range .Principals }}
  - provider: whatsapp
    external_user_id: ${{"{"}}{{ .ExternalVar }}{{"}"}}
    principal_id: {{ .ID }}
{{- end }}

channels:
{{- range .Channels }}
  - provider: whatsapp
    channel_id: ${{"{"}}{{ .IDVar }}{{"}"}}
    kind: {{ .Kind }}
    org_id: {{ $.OrgID }}
    scope: {{ .Scope }}
    scope_id: {{ .ScopeID }}
{{- if .PrincipalID }}
    principal_id: {{ .PrincipalID }}
{{- end }}
{{- if .Members }}
    # Dans un groupe, un message sans mention explicite est ignoré avant tout
    # appel au modèle.
    activation: mention
    members:
{{- range .Members }}
      - {{ . }}
{{- end }}
{{- end }}
{{- if .Resources }}
    # Identifiants des ressources externes : résolus par l'application, jamais
    # exposés au modèle ni acceptés de sa part. Chaque clé correspond à un
    # mcp_servers.<nom>.resource.key.
    resources:
{{- range $key, $var := .Resources }}
      {{ $key }}: ${{"{"}}{{ $var }}{{"}"}}
{{- end }}
{{- end }}
{{- end }}
{{- if .Schedule }}

schedules:
  - id: {{ .Schedule.ID }}
    enabled: true
    schedule:
      cron: "{{ .Schedule.Cron }}"
      # Fuseau explicite obligatoire. Éviter une heure entre 02:00 et 02:59 :
      # c'est la plage rejouée au retour à l'heure d'hiver.
      timezone: {{ .Schedule.Timezone }}
    execution:
      principal_id: scheduler-readonly
      org_id: {{ .OrgID }}
      scope: org
      scope_id: {{ .OrgID }}
      agent: main
      prompt: |
        {{ .Schedule.Prompt }}
      # read_only : les actions proposées sont ignorées. require_confirmation
      # les transforme en plan qu'un humain habilité peut confirmer.
      actions:
        policy: read_only
    delivery:
      provider: whatsapp
      channel_id: ${{"{"}}{{ .Schedule.ChannelID }}{{"}"}}
      mode: on_content
    concurrency:
      policy: forbid
      timeout: 10m
{{- end }}
`

// envTemplate produit le fichier listant les variables à définir.
const envTemplate = `# Variables d'environnement attendues par la configuration Automata.
#
# Copiez ce fichier, renseignez chaque valeur, puis chargez-le avant de
# démarrer. Ne le versionnez pas une fois rempli : il contiendra vos secrets.
#
# Une variable référencée mais absente est une erreur de démarrage, jamais une
# chaîne vide.
{{ range .EnvVars }}
{{ . }}=
{{- end }}
`
