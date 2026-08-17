-- Récurrence des rappels ponctuels : un rappel peut porter une expression
-- cron standard (5 champs, même dialecte que schedules) et un fuseau IANA.
-- Vide = rappel à déclenchement unique (comportement historique).
--
-- Un rappel récurrent reste au statut pending après chaque envoi : le
-- dispatcher (internal/reminder) recalcule fire_at sur l'occurrence suivante
-- au lieu de le classer sent. Seule une annulation (ou un échec de
-- livraison) termine la série. Le fuseau est stocké pour que « chaque mardi
-- 20h » reste 20h à travers les changements d'heure — un décalage fixe ne le
-- garantirait pas.
ALTER TABLE reminders ADD COLUMN recurrence TEXT NOT NULL DEFAULT '';
ALTER TABLE reminders ADD COLUMN timezone TEXT NOT NULL DEFAULT '';
