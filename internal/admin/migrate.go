package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

type Migrator struct {
	API       kube.API
	BackupDir string
	Log       io.Writer
	Wait      func(context.Context) error
}

func (m *Migrator) get(ctx context.Context, path string) (Object, error) {
	r, err := m.API.Request(ctx, "GET", path, nil, "")
	var api *kube.Error
	if errors.As(err, &api) && api.Code == 404 {
		return nil, nil
	}
	return r, err
}
func (m *Migrator) items(ctx context.Context, path string) ([]any, error) {
	r, err := m.get(ctx, path)
	if err != nil {
		return nil, err
	}
	return jsonx.Array(r["items"]), nil
}
func secretsPath(ns string) string { return "/api/v1/namespaces/" + ns + "/secrets" }
func findSources(items []any) (states []Object, auth Object) {
	for _, v := range items {
		s := jsonx.Obj(v)
		name := jsonx.Str(jsonx.Obj(s["metadata"])["name"])
		if strings.HasPrefix(name, "tfstate-") {
			states = append(states, s)
		}
		if name == "gateway-auth" {
			auth = s
		}
	}
	return
}
func (m *Migrator) Preflight(ctx context.Context) error {
	items, err := m.items(ctx, secretsPath("infra-runners"))
	if err != nil {
		return err
	}
	states, auth := findSources(items)
	if len(states) > 0 {
		return fmt.Errorf("legacy state exists; follow docs/operations.de.md and run make migrate-state first")
	}
	if auth != nil {
		return m.CopySecret(ctx, auth, "infra-gateway")
	}
	return nil
}
func (m *Migrator) pause(ctx context.Context) error {
	for _, name := range []string{"database-hook", "platform-gateway"} {
		path := "/apis/apps/v1/namespaces/infra-runners/deployments/" + name
		deployment, err := m.get(ctx, path)
		if err != nil {
			return err
		}
		if deployment == nil {
			continue
		}
		if _, err = m.API.Request(ctx, "PATCH", path+"/scale", Object{"spec": Object{"replicas": 0}}, "application/merge-patch+json"); err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		err = m.waitPods(waitCtx, "/api/v1/namespaces/infra-runners/pods?labelSelector=app%3D"+name)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
func (m *Migrator) waitPods(ctx context.Context, path string) error {
	for {
		pods, err := m.items(ctx, path)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return nil
		}
		if m.Wait != nil {
			if err := m.Wait(ctx); err != nil {
				return err
			}
		} else {
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}
func (m *Migrator) Migrate(ctx context.Context) error {
	if err := m.pause(ctx); err != nil {
		return err
	}
	for _, team := range Teams {
		jobs, err := m.items(ctx, "/apis/batch/v1/namespaces/"+team+"/jobs")
		if err != nil {
			return err
		}
		for _, v := range jobs {
			job := jsonx.Obj(v)
			terminal := false
			for _, v := range jsonx.Array(jsonx.Obj(job["status"])["conditions"]) {
				c := jsonx.Obj(v)
				if c["status"] == "True" && (c["type"] == "Complete" || c["type"] == "Failed") {
					terminal = true
				}
			}
			if !terminal {
				return fmt.Errorf("a team Job is still active; wait and rerun migration; hooks remain stopped")
			}
		}
	}
	leases, err := m.items(ctx, "/apis/coordination.k8s.io/v1/namespaces/infra-runners/leases")
	if err != nil {
		return err
	}
	for _, v := range leases {
		if jsonx.Str(jsonx.Obj(jsonx.Obj(v)["spec"])["holderIdentity"]) != "" {
			return fmt.Errorf("a legacy Lease is held; resolve the lock; hooks remain stopped")
		}
	}
	items, err := m.items(ctx, secretsPath("infra-runners"))
	if err != nil {
		return err
	}
	states, auth := findSources(items)
	dir := m.BackupDir
	if dir == "" {
		dir = ".local"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	backup := filepath.Join(dir, fmt.Sprintf("state-backup-%d.json", time.Now().UnixNano()))
	file, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(jsonx.Encode(Object{"states": states, "auth": auth}))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	for _, state := range states {
		name := jsonx.Str(jsonx.Obj(state["metadata"])["name"])
		team := ""
		for _, t := range Teams {
			if strings.HasPrefix(name, "tfstate-default-"+t+"-") {
				team = t
				break
			}
		}
		if team == "" {
			return fmt.Errorf("unrecognized state identity; migrate manually; hooks remain stopped")
		}
		if err := m.CopySecret(ctx, state, "panko-state-"+team); err != nil {
			return err
		}
	}
	if auth != nil {
		if err := m.CopySecret(ctx, auth, "infra-gateway"); err != nil {
			return err
		}
	}
	for _, state := range states {
		meta := jsonx.Obj(state["metadata"])
		preconditions := Object{}
		for _, k := range []string{"uid", "resourceVersion"} {
			if v, ok := meta[k]; ok {
				preconditions[k] = v
			}
		}
		_, err := m.API.Request(ctx, "DELETE", secretsPath("infra-runners")+"/"+jsonx.Str(meta["name"]), Object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": preconditions}, "")
		var api *kube.Error
		if err != nil && !(errors.As(err, &api) && api.Code == 404) {
			return err
		}
	}
	if m.Log != nil {
		fmt.Fprintln(m.Log, "State migration verified. Private backup:", backup)
		fmt.Fprintln(m.Log, "Controllers remain stopped. Build images and run make deploy; do not restart old runners.")
	}
	return nil
}
func portable(source Object, namespace string) Object {
	meta := jsonx.Obj(source["metadata"])
	labels := meta["labels"]
	if labels == nil {
		labels = Object{}
	}
	data := source["data"]
	if data == nil {
		data = Object{}
	}
	kind := jsonx.Str(source["type"])
	if kind == "" {
		kind = "Opaque"
	}
	return Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": meta["name"], "namespace": namespace, "labels": labels}, "type": kind, "data": data}
}
func (m *Migrator) CopySecret(ctx context.Context, source Object, namespace string) error {
	ns, err := m.get(ctx, "/api/v1/namespaces/"+namespace)
	if err != nil {
		return err
	}
	if ns == nil {
		if _, err := m.API.Request(ctx, "POST", "/api/v1/namespaces", Object{"apiVersion": "v1", "kind": "Namespace", "metadata": Object{"name": namespace}}, ""); err != nil {
			return err
		}
	}
	desired := portable(source, namespace)
	path := secretsPath(namespace) + "/" + jsonx.Str(jsonx.Obj(desired["metadata"])["name"])
	existing, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	equal := func(actual Object) bool {
		return bytes.Equal(jsonx.Encode(actual["data"]), jsonx.Encode(desired["data"])) && actual["type"] == desired["type"]
	}
	if existing != nil {
		if !equal(existing) {
			return fmt.Errorf("destination Secret differs; resolve conflict without overwriting state")
		}
	} else {
		if _, err := m.API.Request(ctx, "POST", secretsPath(namespace), desired, ""); err != nil {
			return err
		}
	}
	actual, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	if !equal(actual) {
		return fmt.Errorf("destination verification failed; source remains intact")
	}
	return nil
}
