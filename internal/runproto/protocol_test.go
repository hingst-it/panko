package runproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func binding() Binding {
	return Binding{Version: 1, InstallationID: "lab-1", Namespace: "team-a", ResourceName: "orders",
		ResourceUID: "resource-uid", Identity: "original-uid", RunUID: "run-uid", RunName: "run-1",
		Attempt: 2, Action: "apply", AgentID: "agent-1", LeaseEpoch: 3}
}

func point() *Checkpoint {
	return &Checkpoint{Lineage: "lineage-1", Serial: 0, Digest: strings.Repeat("a", 64)}
}

func commit() Commit {
	return Commit{Binding: binding(), IdempotencyKey: "0123456789abcdef", Changes: &Changes{},
		Checkpoint: point(), FinishedAt: "2026-10-24T12:00:00Z"}
}

func claim() Claim {
	return Claim{Binding: binding(), Generation: 1, ModuleRepository: "https://example.org/module.git",
		ModuleRevision: strings.Repeat("a", 40), SpecSnapshot: json.RawMessage(`{"nested":{"a":null}}`),
		BackendBinding: json.RawMessage(`{}`), LeaseDurationSeconds: 60, HeartbeatIntervalSeconds: 15,
		Deadline: "2026-10-24T13:00:00Z", ExpectedCheckpoint: point()}
}

func artifact() json.RawMessage {
	return json.RawMessage(`{"version":1,"identity":"original-uid","uid":"resource-uid","generatedAt":"2026-10-24T12:00:00Z","nodes":[{"id":"n1","label":"postgresql_role.owner","kind":"resource"}],"edges":[]}`)
}

func wantError(t *testing.T, err, kind error) {
	t.Helper()
	var typed *ValidationError
	if !errors.Is(err, kind) || !errors.As(err, &typed) {
		t.Fatalf("got %v, want typed %v", err, kind)
	}
}

func TestBinding(t *testing.T) {
	b := binding()
	if err := ValidateHeartbeat(Heartbeat{b}, b); err != nil {
		t.Fatal(err)
	}
	// Every field must be present and match, for both message types.
	for i := 0; i < reflect.TypeFor[Binding]().NumField(); i++ {
		field := reflect.TypeFor[Binding]().Field(i).Name
		t.Run(field+" missing", func(t *testing.T) {
			got := b
			reflect.ValueOf(&got).Elem().Field(i).SetZero()
			wantError(t, ValidateHeartbeat(Heartbeat{got}, b), ErrInvalid)
			c := commit()
			c.Binding = got
			wantError(t, ValidateCommit(c, b), ErrInvalid)
		})
		t.Run(field+" mismatch", func(t *testing.T) {
			got := b
			f := reflect.ValueOf(&got).Elem().Field(i)
			if f.Kind() == reflect.String {
				f.SetString(f.String() + "-other")
			} else {
				f.SetInt(f.Int() + 1)
			}
			if field == "Action" {
				got.Action = "plan"
			}
			kind := ErrInvalid
			if field == "Attempt" || field == "RunUID" {
				kind = ErrStale
			}
			wantError(t, ValidateHeartbeat(Heartbeat{got}, b), kind)
			c := commit()
			c.Binding = got
			wantError(t, ValidateCommit(c, b), kind)
		})
	}
	for _, edit := range []func(*Binding){
		func(b *Binding) { b.LeaseEpoch-- },
		func(b *Binding) { b.Attempt-- },
	} {
		old := b
		edit(&old)
		wantError(t, ValidateHeartbeat(Heartbeat{old}, b), ErrStale)
		c := commit()
		c.Binding = old
		wantError(t, ValidateCommit(c, b), ErrStale)
	}
	for _, edit := range []func(*Binding){
		func(b *Binding) { b.InstallationID = "bad/id" },
		func(b *Binding) { b.AgentID = strings.Repeat("a", 129) },
		func(b *Binding) { b.Namespace = "Team" },
		func(b *Binding) { b.ResourceName = "a..b" },
		func(b *Binding) { b.ResourceUID = "a/b" },
		func(b *Binding) { b.Identity = "a_b" },
		func(b *Binding) { b.RunUID = "a/b" },
		func(b *Binding) { b.RunName = "../run" },
		func(b *Binding) { b.Attempt = -1 },
		func(b *Binding) { b.LeaseEpoch = -1 },
		func(b *Binding) { b.Action = "migrate" },
	} {
		bad := b
		edit(&bad)
		wantError(t, ValidateHeartbeat(Heartbeat{bad}, b), ErrInvalid)
	}
	wantError(t, ValidateHeartbeat(Heartbeat{b}, Binding{}), ErrInvalid)
}

func TestClaimRequest(t *testing.T) {
	valid := ClaimRequest{Version: 1, AgentID: "agent-1", Pool: "remote", Capabilities: []string{"tofu", "s3"}, MaxRuns: 1}
	for _, n := range []int{1, 8} {
		valid.MaxRuns = n
		if err := ValidateClaimRequest(valid); err != nil {
			t.Fatal(err)
		}
	}
	for name, edit := range map[string]func(*ClaimRequest){
		"version":              func(c *ClaimRequest) { c.Version = 2 },
		"agent":                func(c *ClaimRequest) { c.AgentID = "" },
		"pool":                 func(c *ClaimRequest) { c.Pool = "bad/pool" },
		"zero runs":            func(c *ClaimRequest) { c.MaxRuns = 0 },
		"many runs":            func(c *ClaimRequest) { c.MaxRuns = 9 },
		"empty capability":     func(c *ClaimRequest) { c.Capabilities = []string{""} },
		"long capability":      func(c *ClaimRequest) { c.Capabilities = []string{strings.Repeat("a", 65)} },
		"bad capability":       func(c *ClaimRequest) { c.Capabilities = []string{"a/b"} },
		"duplicate capability": func(c *ClaimRequest) { c.Capabilities = []string{"s3", "s3"} },
		"many capabilities":    func(c *ClaimRequest) { c.Capabilities = make([]string, MaxCapabilities+1) },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid
			edit(&c)
			wantError(t, ValidateClaimRequest(c), ErrInvalid)
		})
	}
}

func TestClaim(t *testing.T) {
	for _, p := range []string{"", "modules/postgres", "module..name"} {
		c := claim()
		c.ModulePath = p
		if err := ValidateClaim(c); err != nil {
			t.Fatal(err)
		}
	}
	for name, edit := range map[string]func(*Claim){
		"binding":            func(c *Claim) { c.Version = 0 },
		"generation":         func(c *Claim) { c.Generation = 0 },
		"repository":         func(c *Claim) { c.ModuleRepository = "" },
		"repository control": func(c *Claim) { c.ModuleRepository += "\nsecret" },
		"revision short":     func(c *Claim) { c.ModuleRevision = "abc" },
		"revision nonhex":    func(c *Claim) { c.ModuleRevision = strings.Repeat("z", 40) },
		"spec null":          func(c *Claim) { c.SpecSnapshot = json.RawMessage(`null`) },
		"spec array":         func(c *Claim) { c.SpecSnapshot = json.RawMessage(`[]`) },
		"spec malformed":     func(c *Claim) { c.SpecSnapshot = json.RawMessage(`{`) },
		"spec large":         func(c *Claim) { c.SpecSnapshot = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxSpecBytes) + `"}`) },
		"backend absent":     func(c *Claim) { c.BackendBinding = nil },
		"backend large": func(c *Claim) {
			c.BackendBinding = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxBackendBytes) + `"}`)
		},
		"checkpoint":     func(c *Claim) { c.ExpectedCheckpoint.Digest = "bad" },
		"lease zero":     func(c *Claim) { c.LeaseDurationSeconds = 0 },
		"lease large":    func(c *Claim) { c.LeaseDurationSeconds = 86401 },
		"heartbeat zero": func(c *Claim) { c.HeartbeatIntervalSeconds = 0 },
		"heartbeat late": func(c *Claim) { c.HeartbeatIntervalSeconds = 60 },
		"deadline":       func(c *Claim) { c.Deadline = "yesterday" },
	} {
		t.Run(name, func(t *testing.T) {
			c := claim()
			edit(&c)
			wantError(t, ValidateClaim(c), ErrInvalid)
		})
	}
	for _, p := range []string{"/tmp", "..", "../module", "a/../b", "a//b", "a/./b", "a/", ".", `C:\module`, `a\b`, ".git/hooks", "a b"} {
		t.Run(p, func(t *testing.T) {
			c := claim()
			c.ModulePath = p
			wantError(t, ValidateClaim(c), ErrInvalid)
		})
	}
}

func TestClaimBounds(t *testing.T) {
	for _, limit := range []int{MaxSpecBytes, MaxBackendBytes} {
		for _, extra := range []int{0, 1} {
			c := claim()
			raw := json.RawMessage(`{"x":"` + strings.Repeat("a", limit-8+extra) + `"}`)
			if limit == MaxSpecBytes {
				c.SpecSnapshot = raw
			} else {
				c.BackendBinding = raw
			}
			err := ValidateClaim(c)
			if extra == 0 && err != nil {
				t.Fatal(err)
			}
			if extra == 1 {
				wantError(t, err, ErrInvalid)
			}
		}
	}
	c := claim()
	c.ExpectedCheckpoint = nil
	c.ModuleRevision = strings.Repeat("A", 40)
	if err := ValidateClaim(c); err != nil {
		t.Fatal(err)
	}
	r := ClaimRequest{Version: 1, AgentID: "agent", Pool: "pool", MaxRuns: 1}
	for i := 0; i < MaxCapabilities; i++ {
		r.Capabilities = append(r.Capabilities, fmt.Sprintf("%064d", i))
	}
	if err := ValidateClaimRequest(r); err != nil {
		t.Fatal(err)
	}
}

func TestCommitCombinations(t *testing.T) {
	// All artifact/count combinations for each action and success/failure.
	for _, action := range []string{"apply", "plan", "destroy"} {
		for _, success := range []bool{true, false} {
			for mask := 0; mask < 32; mask++ {
				t.Run(fmt.Sprintf("%s/%t/%d", action, success, mask), func(t *testing.T) {
					c := commit()
					c.Action = action
					if !success {
						c.Code = "provider"
					}
					counts, cp, removed, creds, graph := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0, mask&16 != 0
					c.Changes, c.Checkpoint = nil, nil
					if counts {
						c.Changes = &Changes{Add: math.MaxInt64}
					}
					if cp {
						c.Checkpoint = point()
					}
					c.CheckpointRemoved = removed
					if creds {
						c.Credentials = map[string]string{"password": "test-only"}
					}
					if graph {
						c.Graph = artifact()
					}
					valid := (!success || action == "destroy" || counts) &&
						(!success || action != "apply" || cp) &&
						(!cp || action != "plan" && !(success && action == "destroy")) &&
						removed == (success && action == "destroy") &&
						(!creds || success && action == "apply") && (!graph || success && action == "apply")
					err := ValidateCommit(c, c.Binding)
					if valid && err != nil {
						t.Fatal(err)
					}
					if !valid {
						wantError(t, err, ErrInvalid)
					}
				})
			}
		}
	}
}

func TestCommitFields(t *testing.T) {
	for name, edit := range map[string]func(*Commit){
		"key short":             func(c *Commit) { c.IdempotencyKey = strings.Repeat("a", 15) },
		"key long":              func(c *Commit) { c.IdempotencyKey = strings.Repeat("a", 129) },
		"key characters":        func(c *Commit) { c.IdempotencyKey = "0123456789abcde/" },
		"unknown code":          func(c *Commit) { c.Code = "timeout" },
		"time absent":           func(c *Commit) { c.FinishedAt = "" },
		"time malformed":        func(c *Commit) { c.FinishedAt = "2026-10-24" },
		"add negative":          func(c *Commit) { c.Changes.Add = -1 },
		"change negative":       func(c *Commit) { c.Changes.Change = -1 },
		"remove negative":       func(c *Commit) { c.Changes.Remove = -1 },
		"lineage absent":        func(c *Commit) { c.Checkpoint.Lineage = "" },
		"lineage long":          func(c *Commit) { c.Checkpoint.Lineage = strings.Repeat("a", 129) },
		"serial negative":       func(c *Commit) { c.Checkpoint.Serial = -1 },
		"digest short":          func(c *Commit) { c.Checkpoint.Digest = "abc" },
		"digest nonhex":         func(c *Commit) { c.Checkpoint.Digest = strings.Repeat("z", 64) },
		"credential key empty":  func(c *Commit) { c.Credentials = map[string]string{"": "value"} },
		"credential key long":   func(c *Commit) { c.Credentials = map[string]string{strings.Repeat("a", 65): "value"} },
		"credential key space":  func(c *Commit) { c.Credentials = map[string]string{"db password": "value"} },
		"credential key slash":  func(c *Commit) { c.Credentials = map[string]string{"a/b": "value"} },
		"credential key dot":    func(c *Commit) { c.Credentials = map[string]string{".": "value"} },
		"credential key dots":   func(c *Commit) { c.Credentials = map[string]string{"..password": "value"} },
		"credential value long": func(c *Commit) { c.Credentials = map[string]string{"key": strings.Repeat("a", 4097)} },
		"credentials many": func(c *Commit) {
			c.Credentials = map[string]string{}
			for i := 0; i < 17; i++ {
				c.Credentials[fmt.Sprint(i)] = "v"
			}
		},
		"graph null":    func(c *Commit) { c.Graph = json.RawMessage(`null`) },
		"graph invalid": func(c *Commit) { c.Graph = json.RawMessage(`{}`) },
		"graph unknown": func(c *Commit) {
			c.Graph = bytes.Replace(artifact(), []byte(`"nodes":`), []byte(`"secret":"value","nodes":`), 1)
		},
		"graph nested unknown": func(c *Commit) {
			c.Graph = bytes.Replace(artifact(), []byte(`"id":`), []byte(`"secret":"value","id":`), 1)
		},
		"graph identity": func(c *Commit) { c.Graph = bytes.ReplaceAll(artifact(), []byte(`original-uid`), []byte(`other`)) },
		"graph uid":      func(c *Commit) { c.Graph = bytes.ReplaceAll(artifact(), []byte(`resource-uid`), []byte(`other`)) },
		"graph edge": func(c *Commit) {
			c.Graph = bytes.Replace(artifact(), []byte(`"edges":[]`), []byte(`"edges":[["n1"]]`), 1)
		},
		"graph label": func(c *Commit) {
			c.Graph = bytes.Replace(artifact(), []byte(`postgresql_role.owner`), []byte(`output.password`), 1)
		},
		"graph large":  func(c *Commit) { c.Graph = append(artifact(), bytes.Repeat([]byte(" "), 64<<10)...) },
		"commit large": func(c *Commit) { c.FinishedAt = strings.Repeat("a", MaxCommitBytes) },
	} {
		t.Run(name, func(t *testing.T) {
			c := commit()
			edit(&c)
			wantError(t, ValidateCommit(c, binding()), ErrInvalid)
		})
	}
	c := commit()
	c.IdempotencyKey = strings.Repeat("_", 128)
	c.Credentials = map[string]string{}
	for i := 0; i < MaxCredentials; i++ {
		c.Credentials[fmt.Sprintf("%064d", i)] = strings.Repeat("a", MaxCredentialValueBytes)
	}
	if err := ValidateCommit(c, c.Binding); err != nil {
		t.Fatal(err)
	}
	for code := range messages {
		c := commit()
		c.Code = code
		if err := ValidateCommit(c, c.Binding); err != nil {
			t.Fatal(code, err)
		}
	}
	if Message("") != "Completed" || Message("arbitrary secret") != "" {
		t.Fatal("message allowlist")
	}
}

func TestDecode(t *testing.T) {
	data, _ := json.Marshal(commit())
	got, err := Decode[Commit](bytes.NewReader(data), int64(len(data)))
	if err != nil || !Same(got, commit()) {
		t.Fatal(got, err)
	}
	for name, data := range map[string][]byte{
		"unknown":           bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"message":"secret"`), 1),
		"unknown nested":    bytes.Replace(data, []byte(`"add":0`), []byte(`"add":0,"other":1`), 1),
		"version":           bytes.Replace(data, []byte(`"version":1`), []byte(`"version":2`), 1),
		"version missing":   bytes.Replace(data, []byte(`"version":1,`), nil, 1),
		"float":             bytes.Replace(data, []byte(`"add":0`), []byte(`"add":0.5`), 1),
		"exponent":          bytes.Replace(data, []byte(`"add":0`), []byte(`"add":1e0`), 1),
		"string number":     bytes.Replace(data, []byte(`"add":0`), []byte(`"add":"1"`), 1),
		"overflow":          bytes.Replace(data, []byte(`"add":0`), []byte(`"add":9223372036854775808`), 1),
		"missing count":     bytes.Replace(data, []byte(`"add":0,`), nil, 1),
		"null count":        bytes.Replace(data, []byte(`"add":0`), []byte(`"add":null`), 1),
		"missing serial":    bytes.Replace(data, []byte(`"serial":0,`), nil, 1),
		"null serial":       bytes.Replace(data, []byte(`"serial":0`), []byte(`"serial":null`), 1),
		"null code":         bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"code":null`), 1),
		"null removal":      bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"checkpointRemoved":null`), 1),
		"null credential":   bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"credentials":{"password":null}`), 1),
		"number credential": bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"credentials":{"password":123}`), 1),
		"trailing":          append(append([]byte{}, data...), []byte(` {}`)...),
		"malformed":         []byte(`{`),
		"null":              []byte(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode[Commit](bytes.NewReader(data), MaxCommitBytes)
			wantError(t, err, ErrInvalid)
		})
	}
	for _, limit := range []int64{0, -1, int64(len(data) - 1)} {
		_, err := Decode[Commit](bytes.NewReader(data), limit)
		wantError(t, err, ErrInvalid)
	}
	for _, n := range []int{MaxCommitBytes, MaxCommitBytes + 1} {
		padded := append(append([]byte{}, data...), bytes.Repeat([]byte(" "), n-len(data))...)
		_, err := Decode[Commit](bytes.NewReader(padded), math.MaxInt64)
		if n == MaxCommitBytes && err != nil {
			t.Fatal(err)
		}
		if n > MaxCommitBytes {
			wantError(t, err, ErrInvalid)
		}
	}
	_, err = Decode[Commit](errorReader{}, MaxCommitBytes)
	wantError(t, err, ErrInvalid)
}

func TestWireMessages(t *testing.T) {
	// Embedded binding fields must round-trip as top-level fields, and opaque
	// claim objects must retain unknown keys and large integer tokens.
	c := claim()
	c.SpecSnapshot = json.RawMessage(`{"custom":9007199254740993}`)
	data, _ := json.Marshal(c)
	got, err := Decode[Claim](bytes.NewReader(data), MaxCommitBytes)
	if err != nil || ValidateClaim(got) != nil || !bytes.Equal(got.SpecSnapshot, c.SpecSnapshot) {
		t.Fatal(got, err)
	}
	h := Heartbeat{binding()}
	data, _ = json.Marshal(h)
	heartbeat, err := Decode[Heartbeat](bytes.NewReader(data), 4096)
	if err != nil || heartbeat != h {
		t.Fatal(heartbeat, err)
	}
	for _, decision := range []string{"continue", "cancel"} {
		response := HeartbeatResponse{Version: 1, Decision: decision}
		data, _ = json.Marshal(response)
		got, err := Decode[HeartbeatResponse](bytes.NewReader(data), 4096)
		if err != nil || got != response {
			t.Fatal(got, err)
		}
	}
	request := ClaimRequest{Version: 1, AgentID: "agent", Pool: "pool", MaxRuns: 1}
	data, _ = json.Marshal(request)
	gotRequest, err := Decode[ClaimRequest](bytes.NewReader(data), 4096)
	if err != nil || ValidateClaimRequest(gotRequest) != nil {
		t.Fatal(gotRequest, err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("secret reader failure") }

func TestSame(t *testing.T) {
	a, b := commit(), commit()
	a.Graph = artifact()
	b.Graph = bytes.Replace(artifact(), []byte(`"version":1,"identity":"original-uid"`), []byte(`"identity": "original-uid", "version": 1`), 1)
	a.Credentials = map[string]string{"a": "1", "b": "2"}
	b.Credentials = map[string]string{"b": "2", "a": "1"}
	if !Same(a, b) {
		t.Fatal("JSON object order changed equality")
	}
	for name, edit := range map[string]func(*Commit){
		"binding":      func(c *Commit) { c.LeaseEpoch++ },
		"key":          func(c *Commit) { c.IdempotencyKey += "a" },
		"result":       func(c *Commit) { c.Code = "provider" },
		"changes":      func(c *Commit) { c.Changes = &Changes{Add: 1} },
		"checkpoint":   func(c *Commit) { c.Checkpoint = point(); c.Checkpoint.Serial++ },
		"removed":      func(c *Commit) { c.CheckpointRemoved = true },
		"credentials":  func(c *Commit) { c.Credentials = map[string]string{"a": "different"} },
		"graph":        func(c *Commit) { c.Graph = nil },
		"finished":     func(c *Commit) { c.FinishedAt = "2026-10-24T12:00:01Z" },
		"invalid JSON": func(c *Commit) { c.Graph = json.RawMessage(`{`) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := b
			edit(&changed)
			if Same(a, changed) {
				t.Fatal("different commit matched")
			}
		})
	}
	a, b = commit(), commit()
	a.Checkpoint.Serial, b.Checkpoint.Serial = 9007199254740992, 9007199254740993
	if Same(a, b) {
		t.Fatal("large integers rounded")
	}
}
