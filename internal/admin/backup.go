package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

const backupVersion = 1

// Pause stops the hook and gateway and waits until no runner Job is active, so state is stable.
func (m *Migrator) Pause(ctx context.Context) error {
	if err := m.pause(ctx); err != nil {
		return err
	}
	for {
		active := false
		for _, team := range Teams {
			jobs, err := m.items(ctx, "/apis/batch/v1/namespaces/"+team+"/jobs")
			if err != nil {
				return err
			}
			for _, item := range jobs {
				active = active || hook.Terminal(jsonx.Obj(item)) == ""
			}
		}
		if !active {
			return nil
		}
		if err := m.pauseBriefly(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

// Resume starts the hook and gateway again.
func (m *Migrator) Resume(ctx context.Context) error {
	for _, target := range [][2]string{{"infra-runners", "database-hook"}, {"infra-gateway", "platform-gateway"}} {
		path := "/apis/apps/v1/namespaces/" + target[0] + "/deployments/" + target[1] + "/scale"
		if err := m.patch(ctx, path, Object{"spec": Object{"replicas": 1}}); err != nil {
			return err
		}
	}
	return nil
}
func (m *Migrator) paused(ctx context.Context) error {
	d, err := m.get(ctx, "/apis/apps/v1/namespaces/infra-runners/deployments/database-hook")
	if err != nil {
		return err
	}
	if d != nil && jsonx.Int(jsonx.Obj(d["spec"])["replicas"]) != 0 {
		return fmt.Errorf("the hook is running; run admin pause first")
	}
	return nil
}

// portableObject keeps what is needed to recreate an object elsewhere and drops server-owned fields.
// List items carry no apiVersion or kind, so the caller names the type.
func portableObject(apiVersion, kind string, item Object) Object {
	meta := jsonx.Obj(item["metadata"])
	kept := Object{"name": meta["name"], "namespace": meta["namespace"]}
	if labels := jsonx.Obj(meta["labels"]); len(labels) > 0 {
		kept["labels"] = labels
	}
	result := Object{"apiVersion": apiVersion, "kind": kind, "metadata": kept}
	for _, key := range []string{"spec", "type", "data"} {
		if value, ok := item[key]; ok {
			result[key] = value
		}
	}
	return result
}

// Export writes everything a restore needs except database contents and S3 objects, which the backup
// script dumps separately: Databases with spec, status and identity, state backend selections and
// profiles, state and checkpoint Secrets, credentials and the installation identity.
func (m *Migrator) Export(ctx context.Context, dir string) error {
	if err := m.paused(ctx); err != nil {
		return err
	}
	installation, err := m.get(ctx, installationPath)
	if err != nil {
		return err
	}
	backup := Object{"version": backupVersion, "createdAt": time.Now().UTC().Format(time.RFC3339), "installation": jsonx.Obj(installation["data"])["id"]}
	databases, backends, profiles, secrets := []any{}, []any{}, []any{}, []any{}
	credentials := map[string]bool{}
	addSecret := func(namespace, name string) error {
		secret, err := m.get(ctx, secretsPath(namespace)+"/"+name)
		if err != nil || secret == nil {
			return err
		}
		secrets = append(secrets, portableObject("v1", "Secret", secret))
		return nil
	}
	for _, team := range Teams {
		items, err := m.items(ctx, "/apis/platform.example.com/v1/namespaces/"+team+"/databases")
		if err != nil {
			return err
		}
		for _, item := range items {
			db := jsonx.Obj(item)
			status := jsonx.Clone(jsonx.Obj(db["status"]))
			if status == nil {
				status = Object{}
			}
			if jsonx.Str(status["identity"]) == "" {
				status["identity"] = jsonx.Obj(db["metadata"])["uid"]
			}
			if ref := jsonx.Str(jsonx.Obj(status["stateBackend"])["credentialsSecret"]); ref != "" {
				credentials[team+"/"+ref] = true
			}
			entry := portableObject("platform.example.com/v1", "Database", db)
			entry["status"] = status
			databases = append(databases, entry)
		}
		for _, kind := range []struct {
			plural, kind string
			list         *[]any
		}{{"statebackends", "StateBackend", &backends}, {"backendprofiles", "BackendProfile", &profiles}} {
			items, err := m.items(ctx, "/apis/platform.example.com/v1/namespaces/"+team+"/"+kind.plural)
			if err != nil {
				return err
			}
			for _, item := range items {
				object := jsonx.Obj(item)
				*kind.list = append(*kind.list, portableObject("platform.example.com/v1", kind.kind, object))
				if ref := jsonx.Str(jsonx.Obj(jsonx.Obj(object["spec"])["credentialsSecretRef"])["name"]); ref != "" {
					credentials[team+"/"+ref] = true
				}
			}
		}
		credentials[team+"/panko-state-s3"] = true
		state, err := m.items(ctx, secretsPath("panko-state-"+team))
		if err != nil {
			return err
		}
		for _, item := range state {
			secrets = append(secrets, portableObject("v1", "Secret", jsonx.Obj(item)))
		}
	}
	for ref := range credentials {
		namespace, name, _ := strings.Cut(ref, "/")
		if err := addSecret(namespace, name); err != nil {
			return err
		}
	}
	for _, ref := range [][2]string{{"infra-backends", "postgres-admin"}, {"infra-backends", "s3-admin"}, {"infra-backends", "s3-teams"}, {"infra-gateway", "gateway-auth"}} {
		if err := addSecret(ref[0], ref[1]); err != nil {
			return err
		}
	}
	backup["databases"], backup["stateBackends"], backup["backendProfiles"], backup["secrets"] = databases, backends, profiles, secrets
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(dir, "cluster.json"), jsonx.Encode(backup)); err != nil {
		return err
	}
	m.logf("Exported %d databases, %d state backends, %d profiles and %d secrets", len(databases), len(backends), len(profiles), len(secrets))
	return nil
}

// Import restores an export into a fresh installation while the hook is paused, so no reconciliation
// can overwrite the restored status. Databases keep their original identity through status.identity.
func (m *Migrator) Import(ctx context.Context, dir string) error {
	if err := m.paused(ctx); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "cluster.json"))
	if err != nil {
		return err
	}
	var backup Object
	if err := jsonx.Decode(bytes.NewReader(data), &backup); err != nil || jsonx.Int(backup["version"]) != backupVersion {
		return fmt.Errorf("unsupported backup format")
	}
	// Validate the whole backup and the destination before anything is written.
	restored := map[string]bool{}
	for _, item := range jsonx.Array(backup["databases"]) {
		db := jsonx.Obj(item)
		meta := jsonx.Obj(db["metadata"])
		identity := jsonx.Str(jsonx.Obj(db["status"])["identity"])
		if jsonx.Str(meta["name"]) == "" || jsonx.Str(meta["namespace"]) == "" || identity == "" || jsonx.Obj(jsonx.Obj(db["status"])["stateBackend"]) == nil {
			return fmt.Errorf("backup contains a Database without name, identity or state binding")
		}
		existing, err := m.get(ctx, databasePath(jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"])))
		if err != nil {
			return err
		}
		if existing == nil {
			continue
		}
		// A Database restored by an interrupted earlier import carries the same identity; anything else is foreign.
		if jsonx.Str(jsonx.Obj(existing["status"])["identity"]) != identity {
			return fmt.Errorf("database %s/%s already exists; restore into a fresh installation", meta["namespace"], meta["name"])
		}
		restored[jsonx.Str(meta["namespace"])+"/"+jsonx.Str(meta["name"])] = true
	}
	// The installation identity is part of every S3 state key.
	if id := jsonx.Str(backup["installation"]); id != "" {
		if err := m.replace(ctx, "/api/v1/namespaces/infra-runners/configmaps", "panko-installation", Object{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": Object{"name": "panko-installation", "namespace": "infra-runners"}, "data": Object{"id": id}}); err != nil {
			return err
		}
	}
	for _, item := range jsonx.Array(backup["secrets"]) {
		secret := jsonx.Obj(item)
		meta := jsonx.Obj(secret["metadata"])
		if err := m.replace(ctx, secretsPath(jsonx.Str(meta["namespace"])), jsonx.Str(meta["name"]), secret); err != nil {
			return err
		}
	}
	for _, kind := range []struct{ key, plural string }{{"backendProfiles", "backendprofiles"}, {"stateBackends", "statebackends"}} {
		for _, item := range jsonx.Array(backup[kind.key]) {
			object := jsonx.Obj(item)
			meta := jsonx.Obj(object["metadata"])
			if err := m.replace(ctx, "/apis/platform.example.com/v1/namespaces/"+jsonx.Str(meta["namespace"])+"/"+kind.plural, jsonx.Str(meta["name"]), object); err != nil {
				return err
			}
		}
	}
	for _, item := range jsonx.Array(backup["databases"]) {
		db := jsonx.Obj(item)
		meta := jsonx.Obj(db["metadata"])
		path := databasePath(jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"]))
		if !restored[jsonx.Str(meta["namespace"])+"/"+jsonx.Str(meta["name"])] {
			if _, err := m.API.Request(ctx, "POST", "/apis/platform.example.com/v1/namespaces/"+jsonx.Str(meta["namespace"])+"/databases", Object{"apiVersion": db["apiVersion"], "kind": db["kind"], "metadata": meta, "spec": db["spec"]}, ""); err != nil {
				return err
			}
		}
		if err := m.restoreStatus(ctx, path, restorableStatus(jsonx.Obj(db["status"]))); err != nil {
			return err
		}
	}
	m.logf("Imported %d databases", len(jsonx.Array(backup["databases"])))
	return nil
}

// restorableStatus keeps identity, binding and the revision state was written with, and drops the
// execution bookkeeping of the old cluster, so each restored Database applies exactly once. That
// apply also recreates connection Secrets, which are not part of the backup.
func restorableStatus(old Object) Object {
	status := Object{"phase": "Pending", "message": "Restored; apply follows"}
	for _, key := range []string{"identity", "stateBackend", "modulePath", "runnerPool", "lastAppliedRevision", "connectionSecretRef"} {
		if value, ok := old[key]; ok {
			status[key] = value
		}
	}
	return status
}

// restoreStatus writes the status with fresh resourceVersions until it sticks, then verifies the identity.
// Metacontroller may add its finalizer concurrently even while the hook is paused.
func (m *Migrator) restoreStatus(ctx context.Context, path string, status Object) error {
	for attempt := 0; attempt < 10; attempt++ {
		current, err := m.get(ctx, path)
		if err != nil {
			return err
		}
		meta := jsonx.Obj(current["metadata"])
		body := Object{"apiVersion": current["apiVersion"], "kind": current["kind"], "metadata": Object{"name": meta["name"], "namespace": meta["namespace"], "resourceVersion": meta["resourceVersion"]}, "status": status}
		_, err = m.API.Request(ctx, "PUT", path+"/status", body, "")
		var api *kube.Error
		if errors.As(err, &api) && api.Code == 409 {
			continue
		}
		if err != nil {
			return err
		}
		written, err := m.get(ctx, path)
		if err != nil {
			return err
		}
		got := jsonx.Obj(written["status"])
		if got["identity"] == status["identity"] && bytes.Equal(jsonx.Encode(got["stateBackend"]), jsonx.Encode(status["stateBackend"])) {
			return nil
		}
	}
	return fmt.Errorf("restored status of %s did not persist", path)
}

// replace creates an object or overwrites the existing one, keeping its resourceVersion.
func (m *Migrator) replace(ctx context.Context, collection, name string, object Object) error {
	existing, err := m.get(ctx, collection+"/"+name)
	if err != nil {
		return err
	}
	if existing == nil {
		_, err = m.API.Request(ctx, "POST", collection, object, "")
		return err
	}
	object = jsonx.Clone(object)
	jsonx.Obj(object["metadata"])["resourceVersion"] = jsonx.Obj(existing["metadata"])["resourceVersion"]
	_, err = m.API.Request(ctx, "PUT", collection+"/"+name, object, "")
	return err
}
