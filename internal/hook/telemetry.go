package hook

import (
	"container/list"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
)

var buckets = []float64{5, 15, 30, 60, 120, 300}

type histogram struct {
	Buckets []int64
	Sum     float64
	Count   int64
}
type blockedEntry struct {
	UID  string
	Seen time.Time
}
type Telemetry struct {
	mu              sync.Mutex
	seen            map[string]bool
	seenOrder       []string
	counts          map[string]int64
	durations       map[string]*histogram
	blocked         map[string]*list.Element
	blockedOrder    *list.List
	Now             func() time.Time
	DeleteThreshold time.Duration
	Log             io.Writer
}

func NewTelemetry(log io.Writer, threshold time.Duration) *Telemetry {
	return &Telemetry{seen: map[string]bool{}, counts: map[string]int64{}, durations: map[string]*histogram{}, blocked: map[string]*list.Element{}, blockedOrder: list.New(), Now: time.Now, DeleteThreshold: threshold, Log: log}
}
func (t *Telemetry) Observe(req Request, response Response) {
	t.mu.Lock()
	defer t.mu.Unlock()
	meta := jsonx.Obj(req.Parent["metadata"])
	now := t.Now()
	for _, child := range Children(req) {
		action := jsonx.Str(Ann(child)[Prefix+"action"])
		if action != "apply" && action != "destroy" {
			continue
		}
		endType := Terminal(child)
		childMeta := jsonx.Obj(child["metadata"])
		key := jsonx.Str(childMeta["uid"])
		if key == "" {
			key = jsonx.Str(meta["uid"]) + "/" + jsonx.Str(childMeta["name"])
		}
		if endType == "" || t.seen[key] {
			continue
		}
		t.seen[key] = true
		t.seenOrder = append(t.seenOrder, key)
		if len(t.seenOrder) > 8192 {
			delete(t.seen, t.seenOrder[0])
			t.seenOrder = t.seenOrder[1:]
		}
		outcome := "failure"
		if endType == "Complete" {
			outcome = "success"
		}
		t.counts[action+"/"+outcome]++
		status := jsonx.Obj(child["status"])
		end := jsonx.Str(status["completionTime"])
		if end == "" {
			for _, item := range jsonx.Array(status["conditions"]) {
				c := jsonx.Obj(item)
				if c["type"] == endType && c["status"] == "True" {
					end = jsonx.Str(c["lastTransitionTime"])
					break
				}
			}
		}
		start, errStart := time.Parse(time.RFC3339Nano, jsonx.Str(status["startTime"]))
		finish, errEnd := time.Parse(time.RFC3339Nano, end)
		if errStart == nil && errEnd == nil {
			duration := finish.Sub(start).Seconds()
			if duration < 0 {
				duration = 0
			}
			h := t.durations[action]
			if h == nil {
				h = &histogram{Buckets: make([]int64, len(buckets))}
				t.durations[action] = h
			}
			h.Count++
			h.Sum += duration
			for i, bound := range buckets {
				if duration <= bound {
					h.Buckets[i]++
				}
			}
		}
	}
	uid := jsonx.Str(meta["uid"])
	deleted, err := time.Parse(time.RFC3339Nano, jsonx.Str(meta["deletionTimestamp"]))
	done := response.Finalized != nil && *response.Finalized
	if err == nil && !done && now.Sub(deleted) >= t.DeleteThreshold {
		if element := t.blocked[uid]; element != nil {
			element.Value = blockedEntry{uid, now}
			t.blockedOrder.MoveToBack(element)
		} else {
			t.blocked[uid] = t.blockedOrder.PushBack(blockedEntry{uid, now})
		}
		if t.blockedOrder.Len() > 8192 {
			element := t.blockedOrder.Front()
			delete(t.blocked, element.Value.(blockedEntry).UID)
			t.blockedOrder.Remove(element)
		}
	} else if element := t.blocked[uid]; element != nil {
		delete(t.blocked, uid)
		t.blockedOrder.Remove(element)
	}
	before := jsonx.Obj(req.Parent["status"])
	changed := false
	for _, k := range []string{"phase", "message", "observedGeneration", "destroy"} {
		if string(jsonx.Encode(before[k])) != string(jsonx.Encode(response.Status[k])) {
			changed = true
		}
	}
	if changed && t.Log != nil {
		var job any
		action := "apply"
		if jsonx.Str(meta["deletionTimestamp"]) != "" {
			action = "destroy"
		}
		generation := jsonx.Int(response.Status["observedGeneration"])
		if len(response.Children) > 0 {
			child := response.Children[0]
			job = jsonx.Obj(child["metadata"])["name"]
			if a := jsonx.Str(Ann(child)[Prefix+"action"]); a != "" {
				action = a
			}
			if g, e := strconv.ParseInt(jsonx.Str(Ann(child)[Prefix+"generation"]), 10, 64); e == nil {
				generation = g
			}
		}
		_, _ = fmt.Fprintln(t.Log, string(jsonx.Encode(Object{"event": "reconcile", "uid": uid, "namespace": meta["namespace"], "name": meta["name"], "generation": generation, "action": action, "job": job, "phase": response.Status["phase"]})))
	}
}
func (t *Telemetry) Render() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.Now()
	for element := t.blockedOrder.Front(); element != nil; {
		next := element.Next()
		entry := element.Value.(blockedEntry)
		if now.Sub(entry.Seen) >= 120*time.Second {
			delete(t.blocked, entry.UID)
			t.blockedOrder.Remove(element)
		}
		element = next
	}
	var b strings.Builder
	b.WriteString("# TYPE panko_execution_total counter\n")
	keys := []string{}
	for k := range t.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.Split(k, "/")
		fmt.Fprintf(&b, "panko_execution_total{action=%q,outcome=%q} %d\n", parts[0], parts[1], t.counts[k])
	}
	b.WriteString("# TYPE panko_execution_duration_seconds histogram\n")
	for _, action := range []string{"apply", "destroy"} {
		h := t.durations[action]
		if h == nil {
			continue
		}
		for i, bound := range buckets {
			fmt.Fprintf(&b, "panko_execution_duration_seconds_bucket{action=%q,le=%q} %d\n", action, strconv.FormatFloat(bound, 'f', -1, 64), h.Buckets[i])
		}
		fmt.Fprintf(&b, "panko_execution_duration_seconds_bucket{action=%q,le=\"+Inf\"} %d\npanko_execution_duration_seconds_sum{action=%q} %g\npanko_execution_duration_seconds_count{action=%q} %d\n", action, h.Count, action, h.Sum, action, h.Count)
	}
	fmt.Fprintf(&b, "# TYPE panko_blocked_deletions gauge\npanko_blocked_deletions %d\n", len(t.blocked))
	return b.String()
}
