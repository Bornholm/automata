// Package build porte les informations de version injectées à la
// compilation par goreleaser (-ldflags -X). Sans injection — un
// `go build` local —, les valeurs par défaut disent honnêtement que le
// binaire ne vient pas d'une release.
package build

var (
	// ShortVersion est la version sémantique nue ("1.4.0"), ou "dev".
	ShortVersion = "dev"
	// LongVersion ajoute le commit ("1.4.0 (a1b2c3d)").
	LongVersion = "dev (unreleased)"
)
