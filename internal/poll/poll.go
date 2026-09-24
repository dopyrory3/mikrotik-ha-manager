// Package poll runs per-router polling goroutines that emit snapshot
// messages consumed by the UI's root model.
package poll

import (
	"context"
	"time"

	"mtha/internal/routeros"
)

// RouterKey identifies which side of a pair a snapshot belongs to ("a" or "b").
type RouterKey string

// Snapshot is a point-in-time read of one router's status.
type Snapshot struct {
	Router    RouterKey
	Reachable bool
	Err       error

	Resource *routeros.SystemResource
	Identity *routeros.Identity
	VRRP     []routeros.VRRPInstance
	Netwatch []routeros.NetwatchEntry

	PolledAt time.Time
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
	snap.Reachable = true
	snap.Resource = resource

	if id, err := p.Client.Identity(ctx); err == nil {
		snap.Identity = id
	}
	if vrrp, err := p.Client.VRRP(ctx); err == nil {
		snap.VRRP = vrrp
	}
	if nw, err := p.Client.Netwatch(ctx); err == nil {
		snap.Netwatch = nw
	}

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
