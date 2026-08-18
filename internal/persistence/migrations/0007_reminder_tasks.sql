-- Tâches planifiées créées conversationnellement (outil schedule_task,
-- internal/agent). Une tâche réutilise entièrement la mécanique des rappels
-- — échéance, récurrence cron, fuseau, annulation, cloisonnement par
-- conversation — et n'en diffère que sur un point : à l'échéance, au lieu
-- d'envoyer message tel quel, le dispatcher le donne comme consigne à un
-- agent et envoie SA réponse (« un bulletin météo tous les matins »).
--
-- kind = 'message' : rappel historique, texte figé délivré tel quel.
-- kind = 'task'    : message est une consigne, exécutée par agent_id.
--
-- agent_id est l'agent qui a créé la tâche : c'est lui qui l'exécutera. Il
-- est figé à la création plutôt que choisi à l'échéance, pour qu'un
-- changement de configuration ne redirige jamais une tâche existante vers un
-- agent aux capacités différentes.
ALTER TABLE reminders ADD COLUMN kind TEXT NOT NULL DEFAULT 'message';
ALTER TABLE reminders ADD COLUMN agent_id TEXT NOT NULL DEFAULT '';
