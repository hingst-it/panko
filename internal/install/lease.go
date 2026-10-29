package install

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hingst-it/panko/internal/chart"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

const (
	leasePath = "/apis/coordination.k8s.io/v1/namespaces/" + chart.Namespace + "/leases/panko-install"
	microTime = "2006-01-02T15:04:05.000000Z07:00"
)

// leaseDuration is how long a lease stays valid without renewal; tests shorten it.
var leaseDuration = 30 * time.Second

// installLease serializes panko install runs against one cluster. It is renewed while the run
// lasts; a lease that stops being renewed belongs to an interrupted run.
type installLease struct {
	api    kube.API
	holder string
	// Interrupted reports that the previous holder stopped without releasing the lease, so a
	// pending Helm revision it left behind may be recovered.
	Interrupted bool
	stop        context.CancelFunc
	done        chan struct{}
	ctx         context.Context
	cancel      context.CancelCauseFunc
}

// errLeaseLost ends a run whose ownership of the lease is no longer certain.
var errLeaseLost = errors.New("lost the installation lease; this panko install stopped before another one could take over, rerun it")

// Context ends when ownership of the lease can no longer be guaranteed; every mutation of the run
// uses it, including the Helm operation.
func (l *installLease) Context() context.Context { return l.ctx }

// acquireLease takes the lease, waiting up to wait for another run to finish or to expire.
func acquireLease(ctx context.Context, api kube.API, wait time.Duration, out io.Writer) (*installLease, error) {
	id := make([]byte, 4)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	l := &installLease{api: api, holder: fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(id))}
	if err := ensureNamespace(ctx, api); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	announced := false
	// written is the renewTime this run stored; ownership is counted from it, not from the response.
	var written time.Time
	for {
		current, err := api.Request(ctx, "GET", leasePath, nil, "")
		var e *kube.Error
		if errors.As(err, &e) && e.Code == 404 {
			written = time.Now()
			if _, err := api.Request(ctx, "POST", "/apis/coordination.k8s.io/v1/namespaces/"+chart.Namespace+"/leases", l.body(nil, written), ""); err == nil {
				break
			} else if !(errors.As(err, &e) && e.Code == 409) {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		spec := jsonx.Obj(current["spec"])
		holder := jsonx.Str(spec["holderIdentity"])
		renewed, _ := time.Parse(microTime, jsonx.Str(spec["renewTime"]))
		active := holder != "" && time.Since(renewed) < time.Duration(jsonx.Int(spec["leaseDurationSeconds"]))*time.Second
		if !active {
			written = time.Now()
			if _, err := api.Request(ctx, "PUT", leasePath, l.body(current, written), ""); err == nil {
				// An empty holder is a lease released on success; anything else was interrupted.
				l.Interrupted = holder != ""
				break
			} else if !(errors.As(err, &e) && e.Code == 409) {
				return nil, err
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another panko install (%s) is running against this cluster; nothing was changed", holder)
		}
		if !announced {
			fmt.Fprintf(out, "Waiting for another panko install (%s) to finish...\n", holder)
			announced = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	// A slow acquisition leaves less ownership time; past the safe deadline the lease is not used.
	if time.Since(written) >= leaseDuration*2/3 {
		return nil, fmt.Errorf("acquiring the installation lease took %s; the API server is too slow to keep it safely, retry", time.Since(written).Round(time.Second))
	}
	l.ctx, l.cancel = context.WithCancelCause(ctx)
	renewCtx, stop := context.WithCancel(context.Background())
	l.stop, l.done = stop, make(chan struct{})
	go l.renew(renewCtx, written)
	return l, nil
}

func (l *installLease) body(current jsonx.Object, at time.Time) jsonx.Object {
	now := at.UTC().Format(microTime)
	meta := jsonx.Object{"name": "panko-install", "namespace": chart.Namespace}
	if current != nil {
		meta["resourceVersion"] = jsonx.Obj(current["metadata"])["resourceVersion"]
	}
	return jsonx.Object{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": meta,
		"spec": jsonx.Object{"holderIdentity": l.holder, "leaseDurationSeconds": int64(leaseDuration / time.Second), "acquireTime": now, "renewTime": now}}
}

// renew keeps the lease. Ownership is only assumed until two thirds of the lease duration after
// the last successful renewal, well before another run could acquire it: a watchdog cancels the
// run at that deadline even while a renewal request still hangs, and each request is bounded by
// the time that is left. Another holder cancels the run at once.
func (l *installLease) renew(ctx context.Context, renewed time.Time) {
	defer close(l.done)
	duration := leaseDuration
	safe := duration * 2 / 3
	watchdog := time.NewTimer(time.Until(renewed.Add(safe)))
	defer watchdog.Stop()
	// The renewal loop ends with the watchdog, also while a request still hangs.
	ctx, stopRenewals := context.WithCancel(ctx)
	defer stopRenewals()
	renewals := make(chan time.Time)
	lost := make(chan struct{}, 1)
	go func() {
		interval := duration / 6
		last := renewed
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			attempt := time.Now()
			bounded, cancel := context.WithDeadline(ctx, last.Add(safe))
			current, err := l.api.Request(bounded, "GET", leasePath, nil, "")
			if err == nil && jsonx.Str(jsonx.Obj(current["spec"])["holderIdentity"]) != l.holder {
				cancel()
				lost <- struct{}{}
				return
			}
			if err == nil {
				jsonx.Obj(current["spec"])["renewTime"] = attempt.UTC().Format(microTime)
				if _, err = l.api.Request(bounded, "PUT", leasePath, current, ""); err == nil {
					last = attempt
					select {
					case renewals <- attempt:
					case <-ctx.Done():
						cancel()
						return
					}
				}
			}
			cancel()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-lost:
			l.cancel(errLeaseLost)
			return
		case <-watchdog.C:
			l.cancel(errLeaseLost)
			return
		case at := <-renewals:
			if !watchdog.Stop() {
				select {
				case <-watchdog.C:
				default:
				}
			}
			watchdog.Reset(time.Until(at.Add(safe)))
		}
	}
}

// release ends the run: the holder is cleared, so the next run knows this one finished.
func (l *installLease) release(ctx context.Context) {
	l.stop()
	<-l.done
	current, err := l.api.Request(ctx, "GET", leasePath, nil, "")
	if err != nil || jsonx.Str(jsonx.Obj(current["spec"])["holderIdentity"]) != l.holder {
		return
	}
	jsonx.Obj(current["spec"])["holderIdentity"] = ""
	_, _ = l.api.Request(ctx, "PUT", leasePath, current, "")
}

func ensureNamespace(ctx context.Context, api kube.API) error {
	_, err := api.Request(ctx, "GET", "/api/v1/namespaces/"+chart.Namespace, nil, "")
	var e *kube.Error
	if errors.As(err, &e) && e.Code == 404 {
		_, err = api.Request(ctx, "POST", "/api/v1/namespaces", jsonx.Object{"apiVersion": "v1", "kind": "Namespace", "metadata": jsonx.Object{"name": chart.Namespace}}, "")
		if errors.As(err, &e) && e.Code == 409 {
			return nil
		}
	}
	return err
}
