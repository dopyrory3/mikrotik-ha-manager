// Package poll runs per-router polling goroutines that emit snapshot
// messages consumed by the UI's root model.
package poll

import (
	"context"
	"sync"
	"time"

	"mtha/internal/routeros"
)

// RouterKey identifies which side of a pair a snapshot belongs to ("a" or "b").
type RouterKey string

// Snapshot is a point-in-time read of one router's status.
type Snapshot struct {
	Router RouterKey
	Err    error

	Resource *routeros.SystemResource
	Identity *routeros.Identity
	VRRP     []routeros.VRRPInstance
	Netwatch []routeros.NetwatchEntry

	// IdentityErr, VRRPErr and NetwatchErr record a failure fetching that
	// sub-endpoint separately from Err (which only covers the initial
	// reachability probe). A nil field paired with a nil slice means the
	// endpoint returned no entries, not that the fetch failed; callers
	// that need to tell "empty" from "unknown" must check these.
	IdentityErr error
	VRRPErr     error
	NetwatchErr error

	PolledAt time.Time
}

// Reachable reports whether the initial connectivity probe (system/resource)
// succeeded. It's derived from Err rather than stored separately, since
// poll() only ever sets one of the two. On a zero-value Snapshot (before the
// first poll) this reports true, so callers must gate on "have we polled
// yet" first (see poll.Poller's channel / ui.Model.snapshots) rather than
// trusting Reachable alone.
func (s Snapshot) Reachable() bool {
	return s.Err == nil
}

// Poller periodically snapshots one router and sends the result on C.
type Poller struct {
	Router   RouterKey
	Client   *routeros.Client
	Interval time.Duration

	C chan Snapshot
}

// New creates a Poller for one router. The caller reads snapshots from C.
func New(router RouterKey, client *routeros.Client, interval time.Duration) *Poller {
	return &Poller{
		Router:   router,
		Client:   client,
		Interval: interval,
		C:        make(chan Snapshot, 1),
	}
}

// Run polls at Interval until ctx is cancelled. It should be started in its
// own goroutine.
func (p *Poller) Run(ctx context.Context) {
	p.poll(ctx)

	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	snap := Snapshot{Router: p.Router, PolledAt: time.Now()}

	resource, err := p.Client.SystemResource(ctx)
	if err != nil {
		snap.Err = err
		p.emit(snap)
		return
	}
	snap.Resource = resource

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		if id, err := p.Client.Identity(ctx); err != nil {
			snap.IdentityErr = err
		} else {
			snap.Identity = id
		}
	}()
	go func() {
		defer wg.Done()
		if vrrp, err := p.Client.VRRP(ctx); err != nil {
			snap.VRRPErr = err
		} else {
			snap.VRRP = vrrp
		}
	}()
	go func() {
		defer wg.Done()
		if nw, err := p.Client.Netwatch(ctx); err != nil {
			snap.NetwatchErr = err
		} else {
			snap.Netwatch = nw
		}
	}()
	wg.Wait()

	p.emit(snap)
}

func (p *Poller) emit(snap Snapshot) {
	select {
	case p.C <- snap:
	default:
		// Drop if the consumer hasn't read the previous snapshot yet;
		// the next tick will supersede it.
		select {
		case <-p.C:
		default:
		}
		p.C <- snap
	}
}
