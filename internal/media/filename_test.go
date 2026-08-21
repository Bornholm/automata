package media

import "testing"

// mime.ExtensionsByType trie ses réponses par ordre alphabétique : pour une
// image JPEG, la première est « .jfif », que peu de gens reconnaissent et que
// certaines messageries prévisualisent mal.
func TestDefaultFilenamePrefersTheUsualExtension(t *testing.T) {
	for _, tc := range []struct {
		mimeType string
		expected string
	}{
		{"image/jpeg", "piece-jointe.jpg"},
		{"image/jpeg; charset=binary", "piece-jointe.jpg"},
		{"IMAGE/JPEG", "piece-jointe.jpg"},
		{"text/plain", "piece-jointe.txt"},
		{"image/png", "piece-jointe.png"},
		{"application/pdf", "piece-jointe.pdf"},
		{"type/inconnu", "piece-jointe"},
	} {
		t.Run(tc.mimeType, func(t *testing.T) {
			if got := defaultFilename(tc.mimeType); got != tc.expected {
				t.Errorf("defaultFilename(%q) = %q, attendu %q", tc.mimeType, got, tc.expected)
			}
		})
	}
}
