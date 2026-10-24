package hook

import (
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/statebackend"
)

func TestOptionalMirrorJob(t *testing.T) {
	config := LoadJobConfig()
	config.ProviderMirror = ""
	parent := Object{"metadata": Object{"name": "orders", "namespace": "team-a", "uid": "abc", "generation": 1}, "spec": Object{}}
	create := func() Object {
		job, err := CreateJobWith(config, parent, "apply", "hash", testRevision, statebackend.Fallback("team-a", "orders"), 1, 1, "")
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	original := create()
	if strings.Contains(string(jsonx.Encode(original)), "mirror") {
		t.Fatal("unconfigured Job changed")
	}
	config.ProviderMirror = "https://provider-mirror.infra-runners.svc:8443"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	job := create()
	pod := jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])
	c := jsonx.Obj(jsonx.Array(pod["containers"])[0])
	env := jsonx.Array(c["env"])
	e := jsonx.Obj(env[len(env)-1])
	if e["name"] != "PANKO_PROVIDER_MIRROR" || e["value"] != config.ProviderMirror {
		t.Fatal("mirror env missing")
	}
	mounts := jsonx.Array(c["volumeMounts"])
	mount := jsonx.Obj(mounts[len(mounts)-1])
	if mount["mountPath"] != "/mirror-ca" || mount["readOnly"] != true {
		t.Fatal("unsafe mount")
	}
	volumes := jsonx.Array(pod["volumes"])
	volume := jsonx.Obj(volumes[len(volumes)-1])
	cm := jsonx.Obj(volume["configMap"])
	if cm["name"] != "panko-mirror-ca" || cm["optional"] != true {
		t.Fatal("missing optional CA")
	}
	// Removing just the optional additions must restore the exact old Job.
	c["env"] = env[:len(env)-1]
	c["volumeMounts"] = mounts[:len(mounts)-1]
	pod["volumes"] = volumes[:len(volumes)-1]
	if string(jsonx.Encode(job)) != string(jsonx.Encode(original)) {
		t.Fatal("other Job fields changed")
	}
	for _, address := range []string{"http://mirror", "https://user:secret@mirror", "https://mirror?q=x"} {
		config.ProviderMirror = address
		if config.Validate() == nil {
			t.Fatal("invalid URL accepted")
		}
	}
}
