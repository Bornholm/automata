-- Rattrapage des échéances manquées (panne de réseau, machine éteinte).
--
-- attempts compte les tentatives de livraison infructueuses de l'échéance
-- COURANTE. Un échec ne classe plus l'entrée failed d'office : elle reste
-- pending et sa tentative suivante est reprogrammée avec un délai croissant,
-- tant que la fenêtre de rattrapage est ouverte — sans limite pour une
-- entrée à déclenchement unique (hors plafond de tentatives), jusqu'à
-- l'occurrence suivante pour une récurrente. Remis à zéro à chaque
-- réarmement de récurrence.
ALTER TABLE reminders ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
