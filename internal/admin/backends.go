package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/statebackend"
)

const (
	installationPath = "/api/v1/namespaces/infra-runners/configmaps/panko-installation"
	s3Endpoint       = "http://s3.infra-backends.svc.cluster.local:7070"
)

func databasePath(namespace, name string) string {
	return "/apis/platform.example.com/v1/namespaces/" + namespace + "/databases/" + name
}

// EnsureInstallationID creates the identity that keeps state keys of separate installations apart; it never changes.
func (m *Migrator) EnsureInstallationID(ctx context.Context) error {
	existing, err := m.get(ctx, installationPath)
	if err != nil || existing != nil {
		return err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	_, err = m.API.Request(ctx, "POST", "/api/v1/namespaces/infra-runners/configmaps", Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": Object{"name": "panko-installation", "namespace": "infra-runners"}, "data": Object{"id": hex.EncodeToString(b)}}, "")
	return err
}
func (m *Migrator) ensureSecret(ctx context.Context, namespace, name string, data map[string]string) error {
	existing, err := m.get(ctx, secretsPath(namespace)+"/"+name)
	if err != nil || existing != nil {
		return err
	}
	encoded := Object{}
	for key, value := range data {
		encoded[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	_, err = m.API.Request(ctx, "POST", secretsPath(namespace), Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": name, "namespace": namespace}, "data": encoded}, "")
	return err
}

// EnsureS3Auth creates the lab S3 gateway's root key; only onboarding uses it.
func (m *Migrator) EnsureS3Auth(ctx context.Context) error {
	secret, err := randomToken()
	if err != nil {
		return err
	}
	return m.ensureSecret(ctx, "infra-backends", "s3-admin", map[string]string{"access_key_id": "panko-admin", "secret_access_key": secret})
}

// EnsureS3Teams gives every team its own S3 user. Onboarding reads the keys from s3-teams; runners from the team Secret.
func (m *Migrator) EnsureS3Teams(ctx context.Context) error {
	aggregate := Object{}
	for _, team := range Teams {
		secret, err := randomToken()
		if err != nil {
			return err
		}
		if err := m.ensureSecret(ctx, team, "panko-state-s3", map[string]string{"access_key_id": "panko-" + team, "secret_access_key": secret}); err != nil {
			return err
		}
		current, err := m.get(ctx, secretsPath(team)+"/panko-state-s3")
		if err != nil {
			return err
		}
		aggregate[team] = jsonx.Obj(current["data"])["secret_access_key"]
	}
	body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "s3-teams", "namespace": "infra-backends"}, "data": aggregate}
	existing, err := m.get(ctx, secretsPath("infra-backends")+"/s3-teams")
	if err != nil {
		return err
	}
	if existing == nil {
		_, err = m.API.Request(ctx, "POST", secretsPath("infra-backends"), body, "")
	} else {
		_, err = m.API.Request(ctx, "PUT", secretsPath("infra-backends")+"/s3-teams", body, "")
	}
	return err
}

// S3Profiles publishes one approved destination per team: its own bucket, reachable only with its own key.
func S3Profiles() Object {
	items := []any{}
	for _, team := range Teams {
		items = append(items, Object{"apiVersion": statebackend.Group, "kind": "BackendProfile", "metadata": Object{"name": "lab-s3", "namespace": team},
			"spec": Object{"type": "s3", "s3": Object{"bucket": "panko-state-" + team, "region": "us-east-1", "keyPrefix": "panko", "endpoint": s3Endpoint, "usePathStyle": true}}})
	}
	return list(items)
}

// BackendMigration moves one resource's state to the destination of a StateBackend.
type BackendMigration struct{ Namespace, Name, StateBackend, ID string }

// MigrateBackend freezes the resource, copies and verifies the state in a Job, switches the binding
// with one resourceVersion-guarded status update and resumes. Rerunning with the same ID resumes.
func (m *Migrator) MigrateBackend(ctx context.Context, b BackendMigration) error {
	db, err := m.get(ctx, databasePath(b.Namespace, b.Name))
	if err != nil {
		return err
	}
	if db == nil {
		return fmt.Errorf("database %s/%s not found", b.Namespace, b.Name)
	}
	uid := jsonx.Str(jsonx.Obj(db["metadata"])["uid"])
	source := jsonx.Obj(jsonx.Obj(db["status"])["stateBackend"])
	if source == nil {
		return fmt.Errorf("database has no bound state backend yet")
	}
	recordPath := secretsPath("panko-state-"+b.Namespace) + "/panko-migration-" + uid
	record, err := m.loadRecord(ctx, recordPath)
	if err != nil {
		return err
	}
	if record.phase != "" && record.phase != "Resumed" && record.id != b.ID {
		return fmt.Errorf("migration %s is unfinished; resume it with the same ID", record.id)
	}
	if record.id == b.ID && record.phase == "Resumed" {
		m.logf("Migration %s already completed", b.ID)
		return nil
	}
	target := record.target
	if record.id != b.ID {
		if target, err = m.resolveTarget(ctx, db, b.StateBackend); err != nil {
			return err
		}
		if bytes.Equal(jsonx.Encode(target), jsonx.Encode(source)) {
			return fmt.Errorf("database already uses this state backend")
		}
		record = migrationRecord{id: b.ID, source: source, target: target}
	}
	save := func(phase string) error {
		record.phase = phase
		m.logf("Migration %s: %s", b.ID, phase)
		return m.saveRecord(ctx, recordPath, b.Namespace, uid, record)
	}
	if record.phase == "" {
		if err := save("Frozen"); err != nil {
			return err
		}
	}
	if err := m.patch(ctx, databasePath(b.Namespace, b.Name), Object{"metadata": Object{"annotations": Object{hook.Prefix + "state-maintenance": b.ID}}}); err != nil {
		return err
	}
	started := time.Now()
	m.logf("Migration %s: waiting until the hook reports the freeze and no Job is active", b.ID)
	if err := m.waitFrozen(ctx, b, uid); err != nil {
		return err
	}
	m.logf("Migration %s: frozen after %s", b.ID, time.Since(started).Round(time.Second))
	if record.phase == "Frozen" {
		if err := m.runMigrationJob(ctx, db, record); err != nil {
			return fmt.Errorf("%w; the resource stays frozen; inspect the migration Job and rerun with the same ID", err)
		}
		if err := save("Copied"); err != nil {
			return err
		}
	}
	if record.phase == "Copied" {
		if err := m.commit(ctx, b, target); err != nil {
			return err
		}
		if err := save("Committed"); err != nil {
			return err
		}
	}
	// Metacontroller replaces status without a version check, so confirm the binding survived reconciliation.
	if err := m.confirmBinding(ctx, b, target); err != nil {
		return err
	}
	if err := m.patch(ctx, databasePath(b.Namespace, b.Name), Object{"metadata": Object{"annotations": Object{hook.Prefix + "state-maintenance": nil}}}); err != nil {
		return err
	}
	_, _ = m.API.Request(ctx, "DELETE", "/apis/batch/v1/namespaces/"+b.Namespace+"/jobs/"+migrationJobName(uid, b.ID)+"?propagationPolicy=Background", nil, "")
	return save("Resumed")
}

type migrationRecord struct {
	id, phase      string
	source, target Object
}

func (m *Migrator) loadRecord(ctx context.Context, path string) (migrationRecord, error) {
	secret, err := m.get(ctx, path)
	if err != nil || secret == nil {
		return migrationRecord{}, err
	}
	data := jsonx.Obj(secret["data"])
	field := func(key string) []byte {
		value, _ := base64.StdEncoding.DecodeString(jsonx.Str(data[key]))
		return value
	}
	r := migrationRecord{id: string(field("id")), phase: string(field("phase"))}
	if err := jsonx.Decode(bytes.NewReader(field("source")), &r.source); err != nil {
		return r, fmt.Errorf("invalid migration record")
	}
	if err := jsonx.Decode(bytes.NewReader(field("target")), &r.target); err != nil {
		return r, fmt.Errorf("invalid migration record")
	}
	return r, nil
}
func (m *Migrator) saveRecord(ctx context.Context, path, namespace, uid string, r migrationRecord) error {
	body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "panko-migration-" + uid, "namespace": "panko-state-" + namespace},
		"stringData": Object{"id": r.id, "phase": r.phase, "source": string(jsonx.Encode(r.source)), "target": string(jsonx.Encode(r.target))}}
	existing, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	if existing == nil {
		_, err = m.API.Request(ctx, "POST", secretsPath("panko-state-"+namespace), body, "")
	} else {
		_, err = m.API.Request(ctx, "PUT", path, body, "")
	}
	return err
}
func (m *Migrator) resolveTarget(ctx context.Context, db Object, name string) (Object, error) {
	namespace := jsonx.Str(jsonx.Obj(db["metadata"])["namespace"])
	backend, err := m.get(ctx, "/apis/platform.example.com/v1/namespaces/"+namespace+"/statebackends/"+name)
	if err != nil {
		return nil, err
	}
	profiles, err := m.items(ctx, "/apis/platform.example.com/v1/namespaces/"+namespace+"/backendprofiles")
	if err != nil {
		return nil, err
	}
	installation, err := m.get(ctx, installationPath)
	if err != nil {
		return nil, err
	}
	byName := map[string]Object{}
	for _, item := range profiles {
		byName[jsonx.Str(jsonx.Obj(jsonx.Obj(item)["metadata"])["name"])] = jsonx.Obj(item)
	}
	backends := map[string]Object{}
	if backend != nil {
		backends[name] = backend
	}
	selection := jsonx.Clone(db)
	jsonx.Obj(selection["spec"])["stateBackendRef"] = Object{"name": name}
	target, err := statebackend.Resolve(selection, backends, byName, statebackend.Settings{External: true, Installation: jsonx.Str(jsonx.Obj(installation["data"])["id"])})
	if err != nil {
		return nil, err
	}
	meta := jsonx.Obj(db["metadata"])
	return target, statebackend.Validate(target, namespace, jsonx.Str(meta["name"]), jsonx.Str(meta["uid"]))
}
func (m *Migrator) patch(ctx context.Context, path string, body Object) error {
	_, err := m.API.Request(ctx, "PATCH", path, body, "application/merge-patch+json")
	return err
}
func (m *Migrator) pauseBriefly(ctx context.Context, d time.Duration) error {
	if m.Wait != nil {
		return m.Wait(ctx)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// waitFrozen returns once the hook reported the freeze and no Job of the resource is active.
func (m *Migrator) waitFrozen(ctx context.Context, b BackendMigration, uid string) error {
	for {
		db, err := m.get(ctx, databasePath(b.Namespace, b.Name))
		if err != nil {
			return err
		}
		jobs, err := m.items(ctx, "/apis/batch/v1/namespaces/"+b.Namespace+"/jobs?labelSelector="+hook.Prefix+"uid%3D"+uid)
		if err != nil {
			return err
		}
		active := false
		for _, item := range jobs {
			active = active || jsonx.Int(jsonx.Obj(jsonx.Obj(item)["status"])["active"]) > 0 || hook.Terminal(jsonx.Obj(item)) == ""
		}
		message := jsonx.Str(jsonx.Obj(db["status"])["message"])
		if !active && bytes.Contains([]byte(message), []byte("maintenance "+b.ID)) {
			return nil
		}
		if err := m.pauseBriefly(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}
func migrationJobName(uid, id string) string {
	name := "panko-migrate-" + uid[:min(len(uid), 13)] + "-" + id
	return name[:min(len(name), 63)]
}

// runMigrationJob runs the runner's migrate action with the hook's deployed engine and module configuration.
func (m *Migrator) runMigrationJob(ctx context.Context, db Object, r migrationRecord) error {
	meta := jsonx.Obj(db["metadata"])
	namespace, uid := jsonx.Str(meta["namespace"]), jsonx.Str(meta["uid"])
	deployment, err := m.get(ctx, "/apis/apps/v1/namespaces/infra-runners/deployments/database-hook")
	if err != nil || deployment == nil {
		return fmt.Errorf("cannot read the hook configuration")
	}
	env := map[string]string{}
	for _, item := range jsonx.Array(jsonx.Obj(jsonx.Array(jsonx.Obj(jsonx.Obj(jsonx.Obj(deployment["spec"])["template"])["spec"])["containers"])[0])["env"]) {
		env[jsonx.Str(jsonx.Obj(item)["name"])] = jsonx.Str(jsonx.Obj(item)["value"])
	}
	config := hook.JobConfigFrom(func(key string) string { return env[key] })
	generation := jsonx.Int(meta["generation"])
	hash, err := jsonx.Digest(db["spec"], generation)
	if err != nil {
		return err
	}
	job, err := hook.CreateJobWith(config, db, "migrate", hash, config.ModuleRevision, r.source, generation, 1, "")
	if err != nil {
		return err
	}
	// Not a child of the Database: Metacontroller must neither adopt nor delete it.
	name := migrationJobName(uid, r.id)
	job["metadata"] = Object{"name": name, "namespace": namespace, "labels": Object{hook.Prefix + "migration": r.id}}
	container := jsonx.Obj(jsonx.Array(jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])["containers"])[0])
	for _, item := range jsonx.Array(container["env"]) {
		if jsonx.Obj(item)["name"] == "PANKO_JOB" {
			jsonx.Obj(item)["value"] = name
		}
	}
	container["env"] = append(jsonx.Array(container["env"]), Object{"name": "PANKO_TARGET_BACKEND", "value": string(jsonx.Encode(r.target))}, Object{"name": "PANKO_MIGRATION", "value": r.id})
	path := "/apis/batch/v1/namespaces/" + namespace + "/jobs"
	existing, err := m.get(ctx, path+"/"+name)
	if err != nil {
		return err
	}
	if existing == nil {
		if _, err := m.API.Request(ctx, "POST", path, job, ""); err != nil {
			return err
		}
	}
	for {
		current, err := m.get(ctx, path+"/"+name)
		if err != nil {
			return err
		}
		switch hook.Terminal(current) {
		case "Complete":
			return nil
		case "Failed":
			return fmt.Errorf("migration Job %s failed", name)
		}
		if err := m.pauseBriefly(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

// commit is the authority switch: one status update guarded by the observed resourceVersion.
func (m *Migrator) commit(ctx context.Context, b BackendMigration, target Object) error {
	db, err := m.get(ctx, databasePath(b.Namespace, b.Name))
	if err != nil {
		return err
	}
	version := jsonx.Obj(db["metadata"])["resourceVersion"]
	// A JSON patch replaces the binding as a whole (a merge patch would keep old settings) and fails if the resource changed.
	_, err = m.API.Request(ctx, "PATCH", databasePath(b.Namespace, b.Name)+"/status", []any{
		Object{"op": "test", "path": "/metadata/resourceVersion", "value": version},
		Object{"op": "replace", "path": "/status/stateBackend", "value": target}}, "application/json-patch+json")
	return err
}
func (m *Migrator) confirmBinding(ctx context.Context, b BackendMigration, target Object) error {
	want := jsonx.Encode(target)
	stable := 0
	for attempt := 0; attempt < 20 && stable < 3; attempt++ {
		if err := m.pauseBriefly(ctx, 3*time.Second); err != nil {
			return err
		}
		db, err := m.get(ctx, databasePath(b.Namespace, b.Name))
		if err != nil {
			return err
		}
		if bytes.Equal(jsonx.Encode(jsonx.Obj(jsonx.Obj(db["status"])["stateBackend"])), want) {
			stable++
			continue
		}
		// Still frozen, so reasserting the committed binding is safe.
		stable = 0
		if err := m.commit(ctx, b, target); err != nil {
			return err
		}
	}
	if stable < 3 {
		return fmt.Errorf("committed binding did not persist; the resource stays frozen")
	}
	return nil
}
func (m *Migrator) logf(format string, args ...any) {
	if m.Log != nil {
		_, _ = fmt.Fprintf(m.Log, format+"\n", args...)
	}
}
