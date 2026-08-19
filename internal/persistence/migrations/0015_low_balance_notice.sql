-- Date de la dernière alerte de solde bas envoyée à une organisation.
-- Sans elle, chaque passage du débiteur enverrait un nouvel avertissement :
-- prévenir est utile, harceler ferait fuir. Vide = jamais prévenue ; la
-- date est effacée dès que le portefeuille repasse au-dessus du seuil,
-- pour que l'alerte suivante puisse partir.
ALTER TABLE organizations ADD COLUMN low_balance_notified_at TEXT NOT NULL DEFAULT '';
