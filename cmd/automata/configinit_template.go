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

organization:
  id: {{ .OrgID }}
  display_name: {{ .OrgDisplayName }}

storage:
  application:
    driver: sqlite
    path: {{ .DataDir }}/app.sqlite
    pragmas:
      foreign_keys: true
      journal_mode: WAL
      busy_timeout: 5s

courier:
  providers:
    whatsapp:
      type: whatsapp
      # Ce répertoire doit exister et être accessible en écriture avant le
      # premier démarrage. Il conserve la liaison d'appareil : le supprimer
      # oblige à scanner un nouveau QR code.
      session_path: {{ .DataDir }}/courier/whatsapp
{{ if .Observability }}
observability:
  enabled: true
  addr: {{ .ObservabilityAt }}
{{ end }}{{ if .Audio }}
audio:
  enabled: true
  transcription_client: transcription
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
llm_clients:
  main:
    provider: {{ .LLMProvider }}
    model: ${{"{"}}{{ .LLMModelVar }}{{"}"}}
    api_key: ${{"{"}}{{ .LLMKeyVar }}{{"}"}}
    base_url: ${{"{"}}{{ .LLMBaseVar }}{{"}"}}
{{ if .Audio }}
  transcription:
    provider: {{ .LLMProvider }}
    model: ${{"{"}}{{ .AudioModelVar }}{{"}"}}
    api_key: ${{"{"}}{{ .AudioKeyVar }}{{"}"}}
{{ end }}
agents:
  main:
    type: orchestrator
    client: main
    system_prompt:
      file: {{ .PromptsDir }}/main.md
{{- if .Agents }}
    delegates:
{{- range .Agents }}
      - {{ .Name }}
{{- end }}
{{- end }}
    memory:
      search: true
      remember: true
      forget: true
    limits:
      max_sequential_tool_calls: 8
      max_actions_per_turn: 10
      tool_timeout: 30s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 64KiB
{{ range .Agents }}
  {{ .Name }}:
    type: specialist
{{- if .Description }}
    # Repris dans l'outil delegate_to_{{ .Name }} exposé au généraliste :
    # c'est là-dessus qu'il décide de déléguer.
    description: {{ .Description }}
{{- end }}
    client: main
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
    child:
      permissions:
        - memory.personal.read
        - memory.personal.write
        - memory.group.read
        - memory.org.read
        - calendar.personal.read
        - calendar.group.read
        - calendar.org.read
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
