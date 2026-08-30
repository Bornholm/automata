-- Visite d'accueil : où en est chaque membre.
--
-- Valeurs : '' (jamais proposée), 'offered' (proposée, en attente de
-- réponse), 'q1'…'qN' (question en cours), 'done', 'skipped'.
--
-- L'état vit sur le membre plutôt que sur la conversation : quelqu'un qui
-- change de canal ne refait pas la visite, et quelqu'un qui l'a écartée ne
-- se la voit pas reproposer ailleurs. Un accueil qui insiste n'accueille
-- plus.
ALTER TABLE members ADD COLUMN onboarding_state TEXT NOT NULL DEFAULT '';
