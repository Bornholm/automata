package pluginsdk

import (
	"io"
)

// Transport des fichiers entre l'hôte et un plugin.
//
// Les RPC de fichiers et d'objets sont tous bâtis sur le même schéma : une
// première trame de métadonnées, puis des trames de données. Chaque
// implémentation réécrivait sa propre boucle de découpage — et, plus
// coûteux, accumulait les tranches reçues dans un []byte avant de rendre
// la main. Un fichier de 140 Mo se retrouvait alors intégralement en
// mémoire côté plugin ET côté hôte, à chaque transfert simultané.
//
// Les deux fonctions ci-dessous tiennent cette mécanique une fois pour
// toutes, sans rien connaître des types du protocole : l'appelant fournit
// la closure qui emballe ou déballe SA trame. La métadonnée, elle, reste à
// sa charge — elle diffère d'un flux à l'autre.

// ChunkBytes est la taille d'une tranche : 1 Mio, largement sous la limite
// de message gRPC (4 Mio par défaut), et assez grande pour que le coût par
// trame reste négligeable devant les octets transportés.
const ChunkBytes = 1 << 20

// SendFile lit r jusqu'à épuisement et remet ses octets à send, tranche
// par tranche. Le tampon est réutilisé d'une tranche à l'autre : send doit
// donc avoir fini d'en disposer quand elle rend la main — ce qui est le cas
// d'un envoi gRPC, qui sérialise avant de retourner.
func SendFile(send func(data []byte) error, r io.Reader) error {
	buf := make([]byte, ChunkBytes)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			if sendErr := send(buf[:n]); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ChunkWriter rend un io.Writer qui pousse ce qu'on lui écrit dans send,
// par tranches d'au plus ChunkBytes.
//
// C'est le pendant de SendFile pour un contenu PRODUIT au fil de l'eau
// plutôt que lu : une archive zip, par exemple, s'écrit directement dans
// le flux sans jamais exister en entier quelque part.
func ChunkWriter(send func(data []byte) error) io.Writer {
	return &chunkWriter{send: send}
}

type chunkWriter struct {
	send func([]byte) error
}

// Write implémente io.Writer.
func (w *chunkWriter) Write(p []byte) (int, error) {
	written := 0

	for written < len(p) {
		end := min(written+ChunkBytes, len(p))
		if err := w.send(p[written:end]); err != nil {
			return written, err
		}
		written = end
	}

	return written, nil
}

// RecvFile rend un lecteur qui tire les tranches de recv au fil de l'eau.
// recv retourne les octets de la trame suivante, ou io.EOF à la fin du
// flux ; les trames vides sont ignorées.
//
// Rien n'est conservé au-delà de la tranche courante : c'est ce qui permet
// de relayer un fichier de plusieurs centaines de mégaoctets sans le
// matérialiser. Un appelant qui a besoin des octets en entier les lit
// lui-même, avec SA borne (voir io.LimitReader).
func RecvFile(recv func() ([]byte, error)) io.ReadCloser {
	return &chunkReader{recv: recv}
}

// chunkReader adapte une suite de trames en io.Reader.
type chunkReader struct {
	recv func() ([]byte, error)
	// buf porte le reliquat de la tranche courante, jamais plus.
	buf []byte
	// err mémorise la fin du flux : une erreur peut accompagner une
	// dernière tranche utile, qu'il faut alors servir avant de la rendre.
	err error
}

// Read implémente io.Reader.
func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}

		data, err := r.recv()
		if err != nil {
			r.err = err
			if len(data) == 0 {
				return 0, err
			}
		}
		r.buf = data
	}

	n := copy(p, r.buf)
	r.buf = r.buf[n:]

	return n, nil
}

// Close implémente io.Closer. Le flux appartient à celui qui l'a ouvert :
// fermer le lecteur ne l'annule pas, c'est le contexte de l'appel qui s'en
// charge.
func (r *chunkReader) Close() error { return nil }
