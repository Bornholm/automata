package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envVarPattern reconnaît les références ${VARIABLE} dans le YAML brut.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// yamlKeyPattern reconnaît une ligne "clé: valeur" (avec ou sans indicateur
// de liste "- ") afin de reconstituer un chemin YAML approximatif pour les
// messages d'erreur.
var yamlKeyPattern = regexp.MustCompile(`^(\s*)(-\s+)?([A-Za-z0-9_.\-]+):(.*)$`)

// MissingEnvVarError signale une référence ${VARIABLE} non résolue.
type MissingEnvVarError struct {
	Variable string
	Path     string
	Line     int
}

func (e *MissingEnvVarError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("variable d'environnement %q absente (référencée à %s, ligne %d)", e.Variable, e.Path, e.Line)
	}

	return fmt.Sprintf("variable d'environnement %q absente (ligne %d)", e.Variable, e.Line)
}

// expandEnvVars remplace toute occurrence ${VARIABLE} du contenu YAML brut
// par la valeur correspondante de l'environnement. Si une variable
// référencée est absente, une erreur explicite est retournée nommant la
// variable et, dans la mesure du possible, le chemin YAML concerné.
func expandEnvVars(raw []byte) ([]byte, error) {
	lines := strings.Split(string(raw), "\n")

	pathStack := make([]struct {
		indent int
		key    string
	}, 0, 16)

	var missing []error

	for i, line := range lines {
		path := currentPath(pathStack, line)

		updatePathStack(&pathStack, line)

		matches := envVarPattern.FindAllStringSubmatchIndex(line, -1)
		if len(matches) == 0 {
			continue
		}

		var sb strings.Builder

		last := 0

		for _, m := range matches {
			name := line[m[2]:m[3]]

			sb.WriteString(line[last:m[0]])

			value, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, &MissingEnvVarError{
					Variable: name,
					Path:     path,
					Line:     i + 1,
				})

				sb.WriteString(line[m[0]:m[1]])
			} else {
				sb.WriteString(value)
			}

			last = m[1]
		}

		sb.WriteString(line[last:])
		lines[i] = sb.String()
	}

	if len(missing) > 0 {
		return nil, joinErrors(missing)
	}

	return []byte(strings.Join(lines, "\n")), nil
}

// currentPath calcule le chemin YAML pointé par la ligne donnée, en se
// basant sur la pile de clés déjà rencontrées (indentation plus faible).
func currentPath(pathStack []struct {
	indent int
	key    string
}, line string) string {
	m := yamlKeyPattern.FindStringSubmatch(line)
	if m == nil {
		if len(pathStack) == 0 {
			return ""
		}

		return joinPath(pathStack)
	}

	indent := len(m[1])

	parts := make([]string, 0, len(pathStack)+1)
	for _, p := range pathStack {
		if p.indent < indent {
			parts = append(parts, p.key)
		}
	}

	parts = append(parts, m[3])

	return strings.Join(parts, ".")
}

// updatePathStack met à jour la pile de clés en fonction de la ligne
// courante, pour les lignes suivantes.
func updatePathStack(pathStack *[]struct {
	indent int
	key    string
}, line string) {
	m := yamlKeyPattern.FindStringSubmatch(line)
	if m == nil {
		return
	}

	indent := len(m[1])
	key := m[3]

	stack := *pathStack

	// Retirer toutes les entrées de même indentation ou plus profonde.
	n := 0

	for _, p := range stack {
		if p.indent < indent {
			stack[n] = p
			n++
		}
	}

	stack = stack[:n]

	stack = append(stack, struct {
		indent int
		key    string
	}{indent: indent, key: key})

	*pathStack = stack
}

func joinPath(pathStack []struct {
	indent int
	key    string
}) string {
	parts := make([]string, 0, len(pathStack))
	for _, p := range pathStack {
		parts = append(parts, p.key)
	}

	return strings.Join(parts, ".")
}
