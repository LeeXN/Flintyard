package network

import (
	"context"
	"fmt"
	"strconv"

	"github.com/LeeXN/Flintyard/internal/state"
)

// ObserveResource is read-only. It exposes actual ownership/identity evidence
// for gc and restart; a persisted done flag is never treated as kernel truth.
func (m Manager) ObserveResource(ctx context.Context, p Plan, r state.Resource) (map[string]string, bool, error) {
	if err := p.Validate(); err != nil {
		return nil, false, err
	}
	identity, exists, err := m.observe(ctx, p, r)
	if err == nil && exists && r.Kind == "netns" && identity["mounted"] == "true" {
		inode, e := strconv.ParseUint(identity["inode"], 10, 64)
		if e != nil {
			return nil, false, e
		}
		if e = namespaceBusy(ctx, inode); e != nil {
			return nil, false, e
		}
	}
	return identity, exists, err
}

// ReconcileStopped reuses an intact owned network. If some or all runtime
// resources disappeared, it removes only positively owned remnants so the
// caller can rebuild from the unchanged plan. Live namespace members or changed
// identities make Remove fail closed. It never alters disks or reservations.
func (m Manager) ReconcileStopped(ctx context.Context, p Plan, journals []*state.Journal) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	active, present := 0, 0
	coverage := map[string]int{}
	var relevant []*state.Journal
	for _, j := range journals {
		if j.OwnerID != p.EnvironmentID {
			return false, ErrOwnership
		}
		has := false
		for _, r := range j.Resources {
			if !isNetworkKind(r.Kind) || r.State == "released" {
				continue
			}
			has = true
			active++
			coverage[r.Kind]++
			_, exists, err := m.observe(ctx, p, r)
			if err != nil {
				return false, err
			}
			if exists {
				present++
			}
		}
		if has {
			relevant = append(relevant, j)
		}
	}
	complete := coverage["netns"] == 1 && coverage["bridge"] == 1 && coverage["veth"] == 1 && coverage["tap"] == len(p.Nodes) && coverage["nft-ns"] == 1 && coverage["nft-host"] == 1
	if complete && active == present {
		return true, nil
	}
	for _, j := range relevant {
		if _, err := m.Remove(ctx, p, j); err != nil {
			return false, err
		}
	}
	if active == 0 {
		// No journal authorizes adopting a same-name live object. Create will
		// perform its normal collision checks before publishing any new resource.
		return false, nil
	}
	for _, j := range relevant {
		for _, r := range j.Resources {
			if isNetworkKind(r.Kind) && r.State != "released" {
				return false, fmt.Errorf("network reconciliation left an active resource")
			}
		}
	}
	return false, nil
}
