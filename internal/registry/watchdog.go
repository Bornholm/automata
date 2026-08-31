package registry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/alerting"
	"github.com/bornholm/automata/internal/platform"
	"github.com/bornholm/automata/internal/plugin"
)

// La veille d'exploitation : ce qui remarque une panne avant l'utilisateur.
//
// Elle est délibérément grossière — un tour d'inspection toutes les cinq
// minutes, sur des états que le processus tient déjà en mémoire. Une
// détection plus fine n'apporterait rien ici : ce qu'on cherche à attraper,
// c'est un compte de messagerie déconnecté depuis une heure ou un plugin qui
// ne redémarre plus, pas une micro-coupure de trois secondes qui se répare
// toute seule.
//
// L'inspection ne juge que sur la DURÉE d'un état anormal. Un pipeline qui
// redémarre, un plugin relancé, un appairage en cours : tout cela est
// transitoire et n'a pas à réveiller qui que ce soit un dimanche.

// watchdogInterval est la période d'inspection.
const watchdogInterval = 5 * time.Minute

// failureGrace est la durée pendant laquelle un état anormal reste toléré
// sans alerte. Un redémarrage de compte, un plugin relancé, un appairage :
// tous se règlent en bien moins que cela.
const failureGrace = 10 * time.Minute

// probeTimeout borne la sonde de vivacité d'un compte. Self ne fait pas
// d'aller-retour réseau : ce délai n'attend pas un serveur distant, il
// détecte un fournisseur qui ne répond plus du tout — bloqué sur un verrou,
// typiquement. Quelques secondes suffisent et signent le blocage.
const defaultProbeTimeout = 5 * time.Second

// platformStatusSource et pluginStatusSource découpent ce que la veille
// observe, pour qu'elle se teste sans manager réel.
type platformStatusSource interface {
	Statuses() map[string]platform.Status
	Providers() map[string]courier.Provider
}

type pluginStatusSource interface {
	Statuses() []plugin.Status
}

// watchdog inspecte périodiquement l'état de l'instance.
type watchdog struct {
	notifier  *alerting.Notifier
	platforms platformStatusSource
	plugins   pluginStatusSource
	logger    *slog.Logger
	now       func() time.Time
	// probeTimeout borne la sonde ; zéro prend defaultProbeTimeout.
	probeTimeout time.Duration
}

// probeDelay retourne le délai de sonde à appliquer.
func (w *watchdog) probeDelay() time.Duration {
	if w.probeTimeout <= 0 {
		return defaultProbeTimeout
	}
	return w.probeTimeout
}

// run inspecte jusqu'à l'annulation du contexte.
func (w *watchdog) run(ctx context.Context) {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	// La purge est quotidienne, la veille bien plus fréquente : un compteur
	// évite un second minuteur pour si peu.
	var ticks int

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.inspect(ctx)

			ticks++
			if ticks%int(24*time.Hour/watchdogInterval) == 0 {
				w.notifier.Purge(ctx)
			}
		}
	}
}

// inspect fait un tour complet.
func (w *watchdog) inspect(ctx context.Context) {
	w.checkPlatforms(ctx)
	w.probePlatforms(ctx)
	w.checkPlugins(ctx)

	// Les alertes restées en attente repartent ici : c'est le moment où le
	// canal a le plus de chances d'être revenu.
	w.notifier.Flush(ctx)
}

// checkPlatforms alerte sur un compte de messagerie durablement en panne.
func (w *watchdog) checkPlatforms(ctx context.Context) {
	if w.platforms == nil {
		return
	}

	now := w.now()
	for name, status := range w.platforms.Statuses() {
		if status.State != platform.StateFailed {
			continue
		}
		if !status.Since.IsZero() && now.Sub(status.Since) < failureGrace {
			continue
		}

		message := fmt.Sprintf(
			"Alerte Automata : le compte de messagerie « %s » ne répond plus depuis %s. "+
				"Les messages qui y arrivent ne sont pas traités. "+
				"Vérifiez son état dans l'administration, onglet Comptes.",
			name, humanSince(status.Since, now))
		if status.Err != "" {
			message += " Dernière erreur : " + status.Err
		}

		if err := w.notifier.Notify(ctx, alerting.KindPlatformDown, name, message); err != nil {
			w.logger.WarnContext(ctx, "registry: alerte de compte non enregistrée", "platform", name, "error", err)
		}
	}
}

// probePlatforms interroge chaque compte réputé en marche pour vérifier
// qu'il répond encore.
//
// L'état du gestionnaire ne suffit PAS. Il ne devient « en échec » que si le
// pipeline se termine ; un fournisseur bloqué — sur un verrou, sur une
// socket morte — reste indéfiniment « en marche » alors qu'il ne reçoit
// plus rien. C'est exactement ce qui s'est produit le 2026-08-30 avec
// Rocket.Chat : le compte a passé la nuit muet, et rien dans l'instance ne
// pouvait s'en apercevoir.
//
// Self est le seul appel commun à tous les fournisseurs qui traverse leur
// état interne. Il ne prouve pas que le lien distant est vivant, mais il
// prouve que le fournisseur répond — ce qui est précisément ce qui manquait.
func (w *watchdog) probePlatforms(ctx context.Context) {
	if w.platforms == nil {
		return
	}

	statuses := w.platforms.Statuses()

	for name, provider := range w.platforms.Providers() {
		// Un compte à l'arrêt, en appairage ou déjà en échec est traité par
		// checkPlatforms : le sonder n'apprendrait rien et alerterait deux
		// fois pour le même problème.
		if status, ok := statuses[name]; !ok || status.State != platform.StateRunning {
			continue
		}

		selfProvider, ok := provider.(courier.SelfProvider)
		if !ok {
			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, w.probeDelay())
		_, err := selfProvider.Self(probeCtx)
		timedOut := probeCtx.Err() != nil
		cancel()

		if err == nil && !timedOut {
			continue
		}

		reason := "il ne répond plus"
		if timedOut {
			reason = "il ne répond plus (aucune réponse en " + w.probeDelay().String() + ")"
		}

		message := fmt.Sprintf(
			"Alerte Automata : le compte de messagerie « %s » se déclare en marche, mais %s. "+
				"Les messages qui y arrivent ne sont probablement pas traités. "+
				"Un redémarrage du service rétablit généralement la connexion.",
			name, reason)

		if alertErr := w.notifier.Notify(ctx, alerting.KindPlatformMute, name, message); alertErr != nil {
			w.logger.WarnContext(ctx, "registry: alerte de compte muet non enregistrée",
				"platform", name, "error", alertErr)
		}
	}
}

// checkPlugins alerte sur un plugin arrêté.
func (w *watchdog) checkPlugins(ctx context.Context) {
	if w.plugins == nil {
		return
	}

	now := w.now()
	for _, status := range w.plugins.Statuses() {
		if status.Running {
			continue
		}
		// RestartedAt zéro : le plugin n'a jamais démarré. Le cas se
		// signale immédiatement — il ne se réparera pas tout seul.
		if !status.RestartedAt.IsZero() && now.Sub(status.RestartedAt) < failureGrace {
			continue
		}

		message := fmt.Sprintf(
			"Alerte Automata : le plugin « %s » est arrêté. "+
				"Les fonctions qu'il apporte ne répondent plus. "+
				"Vous pouvez le relancer depuis l'administration, onglet Plugins.",
			status.Name)

		if err := w.notifier.Notify(ctx, alerting.KindPluginFailed, status.Name, message); err != nil {
			w.logger.WarnContext(ctx, "registry: alerte de plugin non enregistrée", "plugin", status.Name, "error", err)
		}
	}
}

// humanSince met une durée en mots. Une alerte se lit d'un coup d'œil sur un
// téléphone : « 2 h » y passe mieux que « 2h13m45.6s ».
func humanSince(since time.Time, now time.Time) string {
	if since.IsZero() {
		return "un moment"
	}

	elapsed := now.Sub(since)
	switch {
	case elapsed < time.Hour:
		return fmt.Sprintf("%d minutes", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%d heures", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%d jours", int(elapsed.Hours()/24))
	}
}
