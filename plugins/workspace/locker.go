package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Le casier personnel : un rangement durable, adossé au magasin d'objets
// scellé de l'hôte (migration 0024).
//
// Il existe parce que le bac à sable n'en est pas un. LeaSH purge le
// workspace après son TTL — de l'ordre de la journée : c'est un plan de
// travail, et tout ce qui y traîne finit par disparaître. Une personne qui
// demande « garde-moi ce document » ne demande pas un plan de travail.
//
// Le casier vit dans CE plugin plutôt que dans un plugin dédié parce que les
// objets sont scopés par nom de plugin : un plugin voisin ne pourrait pas
// ranger ce que le workspace vient de produire, or c'est exactement le geste
// attendu. Les deux bouts se rejoignent donc ici, et les octets transitent
// par le plugin sans jamais toucher un disque intermédiaire.

// lockerCollection est la collection d'objets du casier. Une seule suffit :
// la personne range des fichiers, pas une arborescence.
const lockerCollection = "locker"

// lockerMaxBytes borne un fichier rangé, sous la limite par objet de l'hôte
// (16 Mio). Rester en deçà permet de répondre « trop volumineux » ici, avec
// une phrase utile, plutôt que de laisser l'hôte couper le flux.
const lockerMaxBytes = 15 << 20

// lockerTools décrit les outils du casier. Ils n'existent que si l'hôte est
// joignable : sans magasin d'objets, proposer ces outils au modèle lui
// ferait perdre un appel pour s'entendre dire « non câblé ».
func lockerTools() []*proto.ToolDescriptor {
	return []*proto.ToolDescriptor{
		{
			Name: "locker_list",
			Description: "List the files kept in the user's locker. The locker is permanent storage: " +
				"unlike the workspace, nothing there expires.",
			InputSchemaJson: `{"type":"object","properties":{}}`,
			ReadOnly:        true,
			TimeoutSeconds:  60,
		},
		{
			Name: "locker_save",
			Description: "Keep a file of your workspace in the user's locker, permanently. " +
				"Use this whenever the user asks you to keep, store or remember a document. " +
				"Refuses to overwrite: if a file of that name is already in the locker, use locker_replace instead.",
			InputSchemaJson: `{"type":"object","properties":{"path":{"type":"string","description":"Path of the file in your workspace, as returned by list_files."},"name":{"type":"string","description":"Optional name to keep it under; defaults to the file name."}},"required":["path"]}`,
			// read_only : ranger un nouveau fichier dans son propre casier
			// n'écrase rien et ne détruit rien. Ce qui détruit — écraser,
			// supprimer — passe par les deux outils confirmés ci-dessous.
			ReadOnly:       true,
			TimeoutSeconds: 120,
		},
		{
			Name: "locker_get",
			Description: "Copy a file from the user's locker back into your workspace so you can work on it. " +
				"Call locker_list first to see the exact names.",
			InputSchemaJson: `{"type":"object","properties":{"name":{"type":"string","description":"Name of the file in the locker, as returned by locker_list."}},"required":["name"]}`,
			ReadOnly:        true,
			TimeoutSeconds:  120,
		},
		{
			Name: "locker_replace",
			Description: "Replace a file already kept in the user's locker with one from your workspace. " +
				"The previous version is lost.",
			InputSchemaJson: `{"type":"object","properties":{"path":{"type":"string","description":"Path of the file in your workspace."},"name":{"type":"string","description":"Name of the locker file to replace, as returned by locker_list."}},"required":["path","name"]}`,
			// Pas de read_only : écraser détruit la version précédente,
			// l'hôte demande donc confirmation avant d'exécuter.
			TimeoutSeconds: 120,
		},
		{
			Name:            "locker_delete",
			Description:     "Remove a file from the user's locker, permanently.",
			InputSchemaJson: `{"type":"object","properties":{"name":{"type":"string","description":"Name of the file in the locker, as returned by locker_list."}},"required":["name"]}`,
			// Pas de read_only : la suppression est définitive.
			TimeoutSeconds: 60,
		},
	}
}

// lockerName normalise le nom sous lequel un fichier est rangé. Les clés du
// magasin sont bornées à [a-z0-9._/-] côté hôte ; produire ici un nom
// acceptable évite un refus opaque au bout de la chaîne.
func lockerName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(path.Base(raw)))

	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}

	return strings.Trim(sb.String(), "-.")
}

// lockerList liste le casier.
func (p *Plugin) lockerList(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	host := p.hostClient()
	if host == nil {
		return errorOutput("the locker is not available on this instance."), nil
	}

	entries, err := host.ListObjects(ctx, in.Ctx.OrgId, in.Ctx.MemberId, lockerCollection)
	if err != nil {
		slog.WarnContext(ctx, "workspace: listage du casier en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the locker could not be listed: %v", err)), nil
	}
	if len(entries) == 0 {
		return &proto.CallToolOutput{ResultText: "The locker is empty."}, nil
	}

	var sb strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&sb, "%s\t%d bytes\tkept %s\n", entry.Key, entry.Size, entry.UpdatedAt)
	}

	return &proto.CallToolOutput{ResultText: sb.String()}, nil
}

// lockerSave range un fichier du bac à sable. replace commande l'écrasement :
// sans lui, une clé déjà prise est refusée plutôt qu'écrasée en silence.
func (p *Plugin) lockerSave(ctx context.Context, in *proto.CallToolInput, replace bool) (*proto.CallToolOutput, error) {
	host := p.hostClient()
	if host == nil {
		return errorOutput("the locker is not available on this instance."), nil
	}

	var args struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return errorOutput("invalid parameters"), nil
		}
	}

	source := strings.TrimSpace(args.Path)
	if source == "" {
		return errorOutput("parameter 'path' is required"), nil
	}

	target := lockerName(args.Name)
	if target == "" {
		target = lockerName(source)
	}
	if target == "" {
		return errorOutput("the file needs a name made of letters, digits, dots or dashes."), nil
	}

	data, contentType, err := p.leash.GetFile(ctx, in.Ctx.OrgId, in.Ctx.MemberId, source, lockerMaxBytes)
	if err != nil {
		return errorOutput(fmt.Sprintf("no readable file named %s in your workspace: %v. Call list_files to see the exact names.", source, err)), nil
	}

	// L'existence se vérifie ICI, avant d'écrire : l'hôte, lui, remplace
	// sans se poser de question. Le refus doit atteindre le modèle avec le
	// nom de l'outil qui, lui, écrase — sinon il retente à l'identique.
	existing, err := host.ListObjects(ctx, in.Ctx.OrgId, in.Ctx.MemberId, lockerCollection)
	if err != nil {
		return errorOutput(fmt.Sprintf("the locker could not be listed: %v", err)), nil
	}
	for _, entry := range existing {
		if entry.Key != target {
			continue
		}
		if !replace {
			return errorOutput(fmt.Sprintf(
				"%s is already in the locker. Call locker_replace with the same name to overwrite it, or save it under another name.",
				target)), nil
		}
		break
	}

	if err := host.PutObjectSealed(ctx, in.Ctx.OrgId, in.Ctx.MemberId,
		lockerCollection, target, contentType, data); err != nil {
		slog.WarnContext(ctx, "workspace: rangement au casier en échec",
			"org_id", in.Ctx.OrgId, "bytes", len(data), "error", err)
		return errorOutput(fmt.Sprintf("the file could not be kept: %v", err)), nil
	}

	slog.InfoContext(ctx, "workspace: fichier rangé au casier",
		"org_id", in.Ctx.OrgId, "bytes", len(data), "replaced", replace)

	return &proto.CallToolOutput{ResultText: fmt.Sprintf(
		"%s is now kept in the locker (%d bytes). It will still be there in a month.", target, len(data))}, nil
}

// lockerGet ressort un fichier du casier vers le bac à sable.
func (p *Plugin) lockerGet(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	host := p.hostClient()
	if host == nil {
		return errorOutput("the locker is not available on this instance."), nil
	}

	var args struct {
		Name string `json:"name"`
	}
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return errorOutput("invalid parameters"), nil
		}
	}

	target := lockerName(args.Name)
	if target == "" {
		return errorOutput("parameter 'name' is required"), nil
	}

	data, contentType, found, err := host.GetObject(ctx, in.Ctx.OrgId, in.Ctx.MemberId, lockerCollection, target)
	if err != nil {
		slog.WarnContext(ctx, "workspace: lecture du casier en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the locker could not be read: %v", err)), nil
	}
	if !found {
		return errorOutput("no file named " + target + " in the locker. Call locker_list to see the exact names."), nil
	}

	if _, err := p.leash.PutFile(ctx, in.Ctx.OrgId, in.Ctx.MemberId, target, contentType, bytes.NewReader(data)); err != nil {
		slog.WarnContext(ctx, "workspace: dépôt dans le bac à sable en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the file could not be copied into your workspace: %v", err)), nil
	}

	return &proto.CallToolOutput{ResultText: fmt.Sprintf(
		"%s is now in your workspace (%d bytes).", target, len(data))}, nil
}

// lockerDelete retire un fichier du casier.
func (p *Plugin) lockerDelete(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	host := p.hostClient()
	if host == nil {
		return errorOutput("the locker is not available on this instance."), nil
	}

	var args struct {
		Name string `json:"name"`
	}
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return errorOutput("invalid parameters"), nil
		}
	}

	target := lockerName(args.Name)
	if target == "" {
		return errorOutput("parameter 'name' is required"), nil
	}

	deleted, err := host.DeleteObject(ctx, in.Ctx.OrgId, in.Ctx.MemberId, lockerCollection, target)
	if err != nil {
		slog.WarnContext(ctx, "workspace: suppression au casier en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the file could not be removed: %v", err)), nil
	}
	if !deleted {
		return errorOutput("no file named " + target + " in the locker."), nil
	}

	return &proto.CallToolOutput{ResultText: target + " has been removed from the locker."}, nil
}
