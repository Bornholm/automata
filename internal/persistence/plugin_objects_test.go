package persistence_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/persistence"
)

func putObject(t *testing.T, db *persistence.DB, repo *persistence.PluginObjectRepository, collection, key, content string) {
	t.Helper()

	now := time.Now().UTC()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Upsert(context.Background(), tx, persistence.PluginObject{
			PluginName: "pages", OrgID: "org-1", MemberID: "member-1",
			Collection: collection, Key: key,
			ContentType: "text/html", Size: int64(len(content)), Data: []byte(content),
			CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

// L'aller-retour de base : écrire, relire, remplacer, supprimer.
func TestPluginObjectRoundTrip(t *testing.T) {
	db := openTestDB(t, testConfig(t))
	repo := persistence.NewPluginObjectRepository()
	ctx := context.Background()

	putObject(t, db, repo, "spaces/demo/draft", "index.html", "<html>v1</html>")
	putObject(t, db, repo, "spaces/demo/draft", "index.html", "<html>v2</html>")

	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		o, found, err := repo.Get(ctx, tx, "pages", "org-1", "member-1", "spaces/demo/draft", "index.html")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("objet attendu")
		}
		if string(o.Data) != "<html>v2</html>" || o.Size != int64(len(o.Data)) {
			t.Errorf("objet = %+v", o)
		}

		// Le périmètre est étanche : un autre membre ne voit rien.
		if _, found, err := repo.Get(ctx, tx, "pages", "org-1", "member-2", "spaces/demo/draft", "index.html"); err != nil || found {
			t.Errorf("l'objet ne doit pas être visible d'un autre membre (found=%v, err=%v)", found, err)
		}

		deleted, err := repo.Delete(ctx, tx, "pages", "org-1", "member-1", "spaces/demo/draft", "index.html")
		if err != nil || !deleted {
			t.Errorf("Delete = (%v, %v)", deleted, err)
		}
		deleted, err = repo.Delete(ctx, tx, "pages", "org-1", "member-1", "spaces/demo/draft", "index.html")
		if err != nil || deleted {
			t.Errorf("second Delete = (%v, %v)", deleted, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ReplaceCollection vide la cible avant de copier : un fichier supprimé du
// brouillon ne doit pas survivre dans la version publiée.
func TestPluginObjectReplaceCollection(t *testing.T) {
	db := openTestDB(t, testConfig(t))
	repo := persistence.NewPluginObjectRepository()
	ctx := context.Background()

	putObject(t, db, repo, "spaces/demo/draft", "index.html", "nouveau")
	putObject(t, db, repo, "spaces/demo/live", "obsolete.css", "ancien")

	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		copied, err := repo.ReplaceCollection(ctx, tx, "pages", "org-1", "member-1", "spaces/demo/draft", "spaces/demo/live")
		if err != nil {
			return err
		}
		if copied != 1 {
			t.Errorf("copied = %d", copied)
		}

		metas, err := repo.List(ctx, tx, "pages", "org-1", "member-1", "spaces/demo/live")
		if err != nil {
			return err
		}
		if len(metas) != 1 || metas[0].Key != "index.html" {
			t.Errorf("live = %+v", metas)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Usage additionne tailles et compte — l'assiette des quotas — et
// ListCollections filtre par préfixe sans se laisser piéger par les
// métacaractères LIKE.
func TestPluginObjectUsageAndCollections(t *testing.T) {
	db := openTestDB(t, testConfig(t))
	repo := persistence.NewPluginObjectRepository()
	ctx := context.Background()

	putObject(t, db, repo, "spaces/demo/draft", "index.html", strings.Repeat("a", 10))
	putObject(t, db, repo, "spaces/demo/draft", "style.css", strings.Repeat("b", 5))
	putObject(t, db, repo, "imports", "photo.jpg", strings.Repeat("c", 3))

	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		bytes, count, err := repo.Usage(ctx, tx, "pages", "org-1", "member-1")
		if err != nil {
			return err
		}
		if bytes != 18 || count != 3 {
			t.Errorf("usage = (%d, %d)", bytes, count)
		}

		collections, err := repo.ListCollections(ctx, tx, "pages", "org-1", "member-1", "spaces/")
		if err != nil {
			return err
		}
		if len(collections) != 1 || collections[0] != "spaces/demo/draft" {
			t.Errorf("collections = %v", collections)
		}

		// Un préfixe portant un métacaractère LIKE reste littéral.
		collections, err = repo.ListCollections(ctx, tx, "pages", "org-1", "member-1", "sp_ces/")
		if err != nil {
			return err
		}
		if len(collections) != 0 {
			t.Errorf("le préfixe doit être littéral, obtenu %v", collections)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Le slug d'une publication est unique ; la republication passe par Touch,
// la dépublication rend le slug introuvable.
func TestPluginPublicSites(t *testing.T) {
	db := openTestDB(t, testConfig(t))
	repo := persistence.NewPluginPublicSiteRepository()
	ctx := context.Background()

	site := persistence.PluginPublicSite{
		Slug: "x7k2m9p4qr", PluginName: "pages", OrgID: "org-1", MemberID: "member-1",
		Collection: "spaces/demo/live", PublishedAt: time.Now().UTC(),
	}

	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := repo.Insert(ctx, tx, site); err != nil {
			return err
		}

		duplicate := site
		duplicate.Collection = "spaces/autre/live"
		if err := repo.Insert(ctx, tx, duplicate); err == nil {
			t.Error("un slug déjà pris doit être refusé")
		}

		found, ok, err := repo.FindBySlug(ctx, tx, "x7k2m9p4qr")
		if err != nil || !ok || found.Collection != "spaces/demo/live" {
			t.Errorf("FindBySlug = (%+v, %v, %v)", found, ok, err)
		}

		if err := repo.Touch(ctx, tx, "x7k2m9p4qr", time.Now().UTC().Add(time.Hour)); err != nil {
			return err
		}

		existed, err := repo.DeleteByCollection(ctx, tx, "pages", "org-1", "member-1", "spaces/demo/live")
		if err != nil || !existed {
			t.Errorf("DeleteByCollection = (%v, %v)", existed, err)
		}
		if _, ok, err := repo.FindBySlug(ctx, tx, "x7k2m9p4qr"); err != nil || ok {
			t.Errorf("le slug doit être mort après dépublication (ok=%v, err=%v)", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
