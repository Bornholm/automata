-- Suivi des tâches de maintenance périodiques internes (consolidation de la
-- mémoire, etc.) : une ligne par tâche, avec l'horodatage UTC de sa dernière
-- exécution réussie. Permet de respecter une cadence cron à travers les
-- redémarrages du processus : sans cet état, chaque redémarrage relancerait
-- la tâche ou, au contraire, repousserait indéfiniment sa prochaine échéance.
CREATE TABLE maintenance_runs (
    task TEXT PRIMARY KEY,
    last_run_at TEXT NOT NULL
);
