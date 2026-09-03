package plugin

import (
	"context"
	"io"
	"log"
	"log/slog"
	"strings"

	"github.com/hashicorp/go-hclog"
)

// Pont hclog → slog : ce que dit un plugin rejoint le flux de l'hôte.
//
// go-plugin lit la sortie d'erreur du sous-processus et la restitue à
// travers un hclog.Logger — les lignes JSON avec leur niveau, les autres
// en Debug. Sans ce pont, ce logger écrivait directement sur os.Stderr,
// dans son propre format : les journaux des plugins vivaient à côté de
// ceux d'Automata au lieu d'y être, sans horodatage commun ni champs
// communs, et rien ne permettait de les filtrer ensemble.
//
// Le niveau hclog est volontairement réglé au plus bas (Trace) : c'est
// slog qui filtre, une seule fois, avec le niveau de l'instance. Sans
// cela, go-plugin écartait la ligne avant même de la présenter ici — et
// une trace de bibliothèque sur stderr, non structurée, donc classée
// Debug, disparaissait sans laisser de trace au niveau INFO.

// logBridge implémente hclog.Logger en réémettant tout vers slog.
type logBridge struct {
	logger *slog.Logger
	name   string
	// implied porte les paires clé/valeur accumulées par With, dans la
	// forme attendue par hclog (clé, valeur, clé, valeur...).
	implied []any
}

// newLogBridge construit le logger passé à go-plugin pour un plugin donné.
// name est le nom du binaire : il accompagne chaque ligne, seule façon de
// savoir de quel plugin vient une trace quand cinq tournent ensemble.
func newLogBridge(logger *slog.Logger, name string) hclog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return &logBridge{logger: logger.With(slog.String("plugin", name)), name: name}
}

// Log implémente hclog.Logger.
func (b *logBridge) Log(level hclog.Level, msg string, args ...any) {
	b.logger.Log(context.Background(), toSlogLevel(level), msg, b.attrs(args)...)
}

func (b *logBridge) Trace(msg string, args ...any) { b.Log(hclog.Trace, msg, args...) }
func (b *logBridge) Debug(msg string, args ...any) { b.Log(hclog.Debug, msg, args...) }
func (b *logBridge) Info(msg string, args ...any)  { b.Log(hclog.Info, msg, args...) }
func (b *logBridge) Warn(msg string, args ...any)  { b.Log(hclog.Warn, msg, args...) }
func (b *logBridge) Error(msg string, args ...any) { b.Log(hclog.Error, msg, args...) }

// attrs combine les paires accumulées par With et celles de l'appel.
// hclog admet une liste impaire ; slog aussi (il signale la clé
// orpheline), donc rien à corriger ici.
func (b *logBridge) attrs(args []any) []any {
	if len(b.implied) == 0 {
		return args
	}
	out := make([]any, 0, len(b.implied)+len(args))
	out = append(out, b.implied...)
	out = append(out, args...)
	return out
}

func (b *logBridge) IsTrace() bool { return b.enabled(hclog.Trace) }
func (b *logBridge) IsDebug() bool { return b.enabled(hclog.Debug) }
func (b *logBridge) IsInfo() bool  { return b.enabled(hclog.Info) }
func (b *logBridge) IsWarn() bool  { return b.enabled(hclog.Warn) }
func (b *logBridge) IsError() bool { return b.enabled(hclog.Error) }

func (b *logBridge) enabled(level hclog.Level) bool {
	return b.logger.Enabled(context.Background(), toSlogLevel(level))
}

// ImpliedArgs implémente hclog.Logger.
func (b *logBridge) ImpliedArgs() []any { return b.implied }

// With implémente hclog.Logger.
func (b *logBridge) With(args ...any) hclog.Logger {
	next := *b
	next.implied = append(append([]any(nil), b.implied...), args...)
	return &next
}

// Name implémente hclog.Logger.
func (b *logBridge) Name() string { return b.name }

// Named implémente hclog.Logger : go-plugin nomme le sous-logger d'après
// le binaire, ce qui donne « email.email » si l'on empile. Le segment
// ajouté ne sert donc qu'à distinguer les flux internes de go-plugin.
func (b *logBridge) Named(name string) hclog.Logger {
	if b.name == "" {
		return b.ResetNamed(name)
	}
	if strings.HasSuffix(b.name, name) {
		return b
	}
	return b.ResetNamed(b.name + "." + name)
}

// ResetNamed implémente hclog.Logger.
func (b *logBridge) ResetNamed(name string) hclog.Logger {
	next := *b
	next.name = name
	next.logger = b.logger.With(slog.String("plugin_component", name))
	return &next
}

// SetLevel implémente hclog.Logger. Sans effet : le niveau est celui de
// l'instance, décidé une seule fois, et un plugin n'a pas à le changer.
func (b *logBridge) SetLevel(hclog.Level) {}

// GetLevel implémente hclog.Logger. Toujours Trace : c'est ce qui empêche
// go-plugin d'écarter une ligne avant qu'elle n'arrive ici. Le filtrage
// réel appartient à slog.
func (b *logBridge) GetLevel() hclog.Level { return hclog.Trace }

// StandardLogger implémente hclog.Logger.
func (b *logBridge) StandardLogger(opts *hclog.StandardLoggerOptions) *log.Logger {
	return log.New(b.StandardWriter(opts), "", 0)
}

// StandardWriter implémente hclog.Logger : chaque écriture devient une
// ligne de journal, débarrassée du saut de ligne final.
func (b *logBridge) StandardWriter(*hclog.StandardLoggerOptions) io.Writer {
	return &bridgeWriter{bridge: b}
}

type bridgeWriter struct {
	bridge *logBridge
}

func (w *bridgeWriter) Write(p []byte) (int, error) {
	w.bridge.Info(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// toSlogLevel traduit un niveau hclog. hclog.Trace n'a pas d'équivalent :
// il se range sous Debug, un cran plus bas, pour rester distinguable.
func toSlogLevel(level hclog.Level) slog.Level {
	switch level {
	case hclog.Trace:
		return slog.LevelDebug - 4
	case hclog.Debug:
		return slog.LevelDebug
	case hclog.Info, hclog.NoLevel:
		return slog.LevelInfo
	case hclog.Warn:
		return slog.LevelWarn
	case hclog.Error:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

var _ hclog.Logger = (*logBridge)(nil)
