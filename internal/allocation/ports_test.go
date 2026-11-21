package allocation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LeeXN/Flintyard/internal/network"
)

func availablePortTestRange(accounts Accounts) (Range, error) {
	// Hosted CI machines often delegate 200000:210000 in /etc/subuid and
	// /etc/subgid. Pick a bounded range that the actual allocator can use.
	for _, candidate := range []Range{{60000, 64999}, {1000000, 1010000}, {2000000000, 2000010000}} {
		if _, err := ChooseIDs(candidate, accounts, nil, 6); err == nil {
			return candidate, nil
		}
	}
	return Range{}, fmt.Errorf("no unused UID/GID range for the port reservation test")
}

func TestPortTestRangeRespectsDelegatedIDs(t *testing.T) {
	accounts := Accounts{UIDs: map[uint32]bool{}, GIDs: map[uint32]bool{},
		SubUIDs: []Range{{60000, 64999}, {200000, 210000}}, SubGIDs: []Range{{60000, 64999}}}
	r, err := availablePortTestRange(accounts)
	if err != nil || r.First == 60000 {
		t.Fatal("selected a delegated range", r, err)
	}
	if _, err = ChooseIDs(r, accounts, nil, 6); err != nil {
		t.Fatal(err)
	}
}

func TestMappedPortsAcrossRootsAreAtomicAndHostListenerIsRetained(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	coord := Coordinator{RuntimeDir: filepath.Join(t.TempDir(), "runtime")}
	a, b := requestFixture(t), requestFixture(t)
	accounts, err := ReadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if err = addProcessIDs(ctx, &accounts); err != nil {
		t.Fatal(err)
	}
	uidRange, err := availablePortTestRange(accounts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Request{&a, &b} {
		r.UIDRange = uidRange
		r.Network.Connectivity = network.ConnectivityRequest{Forwards: []network.ForwardRequest{{Protocol: "tcp", BindAddress: "127.0.0.1", HostPort: port, Node: r.Network.Nodes[0].Name, GuestPort: 22}}}
	}
	_, err = coord.Reserve(ctx, a)
	if !errors.Is(err, network.ErrPortInUse) {
		t.Fatal("existing host listener was not rejected", err)
	}
	// The rejected allocation must neither close the socket nor publish a key.
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, r := range []Request{a, b} {
		wg.Add(1)
		go func(r Request) { defer wg.Done(); _, err := coord.Reserve(ctx, r); results <- err }(r)
	}
	wg.Wait()
	close(results)
	passed, rejected := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, network.ErrPortInUse) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || rejected != 1 {
		t.Fatal("mapping key was not exclusive", passed, rejected)
	}
	records, err := coord.Reservations(ctx)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	if err = coord.EnsureStopped(ctx, records[0], nil, a.UIDRange); err != nil {
		t.Fatal("intact stopped mapping could not be restored", err)
	}
	if err = coord.Release(ctx, records[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = coord.Reserve(ctx, b); err != nil {
		t.Fatal("released mapping remained reserved", err)
	}
}
