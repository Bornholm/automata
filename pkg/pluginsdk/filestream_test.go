package pluginsdk_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// SendFile découpe en tranches d'au plus ChunkBytes, et n'en perd aucune.
func TestSendFileChunksWithoutLoss(t *testing.T) {
	// Deux tranches pleines et un reste : le cas où une boucle mal écrite
	// oublie la fin.
	payload := bytes.Repeat([]byte("a"), 2*pluginsdk.ChunkBytes+1234)

	var got bytes.Buffer
	sizes := []int{}

	err := pluginsdk.SendFile(func(data []byte) error {
		sizes = append(sizes, len(data))
		got.Write(data)
		return nil
	}, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("%d octets transmis, %d attendus", got.Len(), len(payload))
	}
	for i, size := range sizes {
		if size > pluginsdk.ChunkBytes {
			t.Errorf("tranche %d de %d octets, au-delà de la limite %d", i, size, pluginsdk.ChunkBytes)
		}
	}
}

// Une source qui rend ses octets par petits morceaux ne doit pas produire
// une tranche par appel de Read : c'est le tampon qui décide de la taille.
func TestSendFileWithDribblingReader(t *testing.T) {
	var got bytes.Buffer
	chunks := 0

	err := pluginsdk.SendFile(func(data []byte) error {
		chunks++
		got.Write(data)
		return nil
	}, oneByteReaderOf(strings.Repeat("x", 10)))
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	if got.String() != strings.Repeat("x", 10) {
		t.Errorf("contenu %q", got.String())
	}
	if chunks == 0 {
		t.Error("aucune tranche envoyée")
	}
}

// L'erreur d'envoi remonte immédiatement : inutile de continuer à lire une
// source dont personne ne veut plus les octets.
func TestSendFileStopsOnSendError(t *testing.T) {
	boom := errors.New("flux rompu")

	err := pluginsdk.SendFile(func([]byte) error { return boom }, bytes.NewReader([]byte("abc")))
	if !errors.Is(err, boom) {
		t.Fatalf("erreur %v, attendue %v", err, boom)
	}
}

// RecvFile rend les octets au fil de l'eau, sans jamais réclamer la
// tranche suivante avant d'avoir servi la courante — c'est ce qui borne la
// mémoire à une tranche.
func TestRecvFileStreamsWithoutBuffering(t *testing.T) {
	remaining := [][]byte{[]byte("abc"), []byte("de"), nil, []byte("f")}
	pulled := 0

	body := pluginsdk.RecvFile(func() ([]byte, error) {
		if pulled >= len(remaining) {
			return nil, io.EOF
		}
		data := remaining[pulled]
		pulled++
		return data, nil
	})
	defer func() { _ = body.Close() }()

	// Première lecture : une seule tranche a dû être tirée.
	buf := make([]byte, 3)
	n, err := body.Read(buf)
	if err != nil || string(buf[:n]) != "abc" {
		t.Fatalf("première lecture: %q, %v", string(buf[:n]), err)
	}
	if pulled != 1 {
		t.Errorf("%d tranches tirées, 1 attendue : le lecteur accumule", pulled)
	}

	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// La trame vide est ignorée, pas prise pour une fin de flux.
	if string(rest) != "def" {
		t.Errorf("reste %q, attendu \"def\"", string(rest))
	}
}

// Une erreur accompagnant une dernière tranche utile ne doit pas faire
// perdre ces octets.
func TestRecvFileKeepsDataSentWithError(t *testing.T) {
	done := false

	body := pluginsdk.RecvFile(func() ([]byte, error) {
		if done {
			return nil, io.EOF
		}
		done = true
		return []byte("fin"), io.EOF
	})

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "fin" {
		t.Errorf("données %q, attendues \"fin\"", string(data))
	}
}

// ChunkWriter borne ce qu'il pousse, même sur une écriture massive : c'est
// lui qui protège un producteur au fil de l'eau (une archive zip).
func TestChunkWriterSplitsLargeWrites(t *testing.T) {
	var got bytes.Buffer
	sizes := []int{}

	w := pluginsdk.ChunkWriter(func(data []byte) error {
		sizes = append(sizes, len(data))
		got.Write(data)
		return nil
	})

	payload := bytes.Repeat([]byte("z"), pluginsdk.ChunkBytes+7)
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write: %d octets, %v", n, err)
	}

	if len(sizes) != 2 {
		t.Errorf("%d tranches, 2 attendues", len(sizes))
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Error("contenu altéré")
	}
}

// oneByteReaderOf rend ses octets un par un, comme une connexion lente.
func oneByteReaderOf(s string) io.Reader {
	return &oneByteReader{data: []byte(s)}
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
