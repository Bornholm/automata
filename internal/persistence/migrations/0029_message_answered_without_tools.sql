-- Marque les réponses de l'assistant produites SANS aucun appel d'outil,
-- alors que des outils lui étaient offerts. Une telle réponse ne constate
-- rien : quand elle affirme qu'une chose est impossible ou qu'un service
-- est indisponible, elle l'invente.
--
-- Le fait est enregistré au moment du tour, seul instant où il est connu :
-- rien dans le texte ne permet de le retrouver après coup. Il sert au tour
-- SUIVANT, où l'historique relu porte ce refus et où le modèle l'imite au
-- lieu d'essayer (voir internal/conversation/refusal.go).
--
-- Défaut 0 : tout l'historique déjà stocké est donc réputé « rien à
-- signaler », jamais annoté rétroactivement. C'est le bon sens de lecture —
-- une annotation posée sur un refus légitime dirait au modèle de réessayer
-- ce qui ne marche pas.
ALTER TABLE messages
    ADD COLUMN answered_without_tools INTEGER NOT NULL DEFAULT 0;
