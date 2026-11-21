// Package allocation coordinates local runtime reservations across state roots.
package allocation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
)

type Range struct{ First, Last uint32 }

func ParseRange(s string) (Range, error) {
	var r Range
	p := strings.Split(s, ":")
	if len(p) != 2 {
		return r, fmt.Errorf("UID range must be FIRST:LAST")
	}
	a, e := strconv.ParseUint(p[0], 10, 32)
	if e != nil {
		return r, e
	}
	b, e := strconv.ParseUint(p[1], 10, 32)
	if e != nil {
		return r, e
	}
	if a == 0 || a > b || b >= 1<<32-1 {
		return r, fmt.Errorf("invalid UID range")
	}
	return Range{uint32(a), uint32(b)}, nil
}

type Accounts struct {
	UIDs, GIDs       map[uint32]bool
	SubUIDs, SubGIDs []Range
}

func ReadAccounts() (Accounts, error) {
	a := Accounts{UIDs: map[uint32]bool{}, GIDs: map[uint32]bool{}}
	for _, file := range []struct {
		path string
		ids  map[uint32]bool
	}{{"/etc/passwd", a.UIDs}, {"/etc/group", a.GIDs}} {
		f, err := os.Open(file.path)
		if err != nil {
			return a, err
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := s.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Split(line, ":")
			if len(fields) < 3 {
				f.Close()
				return a, fmt.Errorf("malformed account database")
			}
			id, err := strconv.ParseUint(fields[2], 10, 32)
			if err != nil {
				f.Close()
				return a, err
			}
			file.ids[uint32(id)] = true
		}
		err = s.Err()
		f.Close()
		if err != nil {
			return a, err
		}
	}
	for _, file := range []struct {
		path   string
		target *[]Range
	}{{"/etc/subuid", &a.SubUIDs}, {"/etc/subgid", &a.SubGIDs}} {
		f, err := os.Open(file.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return a, err
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := strings.TrimSpace(s.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			r, err := parseSubordinate(line)
			if err != nil {
				f.Close()
				return a, err
			}
			*file.target = append(*file.target, r)
		}
		err = s.Err()
		f.Close()
		if err != nil {
			return a, err
		}
	}
	return a, nil
}
func parseSubordinate(line string) (Range, error) {
	f := strings.Split(line, ":")
	if len(f) != 3 || f[0] == "" {
		return Range{}, fmt.Errorf("malformed subordinate ID delegation")
	}
	start, err := strconv.ParseUint(f[1], 10, 32)
	if err != nil {
		return Range{}, err
	}
	count, err := strconv.ParseUint(f[2], 10, 32)
	if err != nil || count == 0 || start+count > 1<<32 {
		return Range{}, fmt.Errorf("invalid subordinate ID interval")
	}
	return Range{uint32(start), uint32(start + count - 1)}, nil
}
func blocked(id uint32, ids map[uint32]bool, ranges []Range) bool {
	if ids[id] {
		return true
	}
	for _, r := range ranges {
		if id >= r.First && id <= r.Last {
			return true
		}
	}
	return false
}
func ChooseIDs(r Range, a Accounts, used []uint32, count int) ([]uint32, error) {
	if r.First == 0 || r.First > r.Last || r.Last == ^uint32(0) || count < 1 || count > 128 {
		return nil, fmt.Errorf("invalid ID allocation")
	}
	reserved := map[uint32]bool{}
	for _, id := range used {
		reserved[id] = true
	}
	var result []uint32
	for id := r.First; id <= r.Last; id++ {
		if reserved[id] || blocked(id, a.UIDs, a.SubUIDs) || blocked(id, a.GIDs, a.SubGIDs) {
			continue
		}
		result = append(result, id)
		if len(result) == count {
			return result, nil
		}
	}
	return nil, fmt.Errorf("no unused UID/GID range remains after account, delegation and runtime reservations")
}

type Reservation struct {
	ID          string       `json:"id"`
	RootPath    string       `json:"root_path"`
	OperationID string       `json:"operation_id"`
	Plan        network.Plan `json:"plan"`
}
type registry struct {
	APIVersion string        `json:"api_version"`
	Kind       string        `json:"kind"`
	Records    []Reservation `json:"records"`
}
type Coordinator struct{ RuntimeDir string }
type Request struct {
	Root          state.Root
	OperationID   string
	Network       network.AllocationRequest
	UIDRange      Range
	ReservationID string
	Retained      []Reservation
}

func (c Coordinator) paths() (string, string, error) {
	base := c.RuntimeDir
	if base == "" {
		base = "/run/flintyard"
	}
	if err := state.PrivateDir(base); err != nil {
		return "", "", err
	}
	return filepath.Join(base, "allocations.json"), filepath.Join(base, "allocation.lock"), nil
}
func readRegistry(path string) (registry, error) {
	r := registry{APIVersion: "flintyard/v1alpha1", Kind: "AllocationRegistry", Records: []Reservation{}}
	if err := state.Read(path, &r); err != nil && !os.IsNotExist(err) {
		return r, err
	}
	if r.APIVersion != "flintyard/v1alpha1" || r.Kind != "AllocationRegistry" {
		return r, fmt.Errorf("invalid allocation registry")
	}
	ids, owners, ports := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, record := range r.Records {
		if err := validateReservation(record); err != nil {
			return r, err
		}
		key := record.Plan.RootID + ":" + record.Plan.EnvironmentID
		if ids[record.ID] || owners[key] {
			return r, fmt.Errorf("duplicate allocation record")
		}
		ids[record.ID], owners[key] = true, true
		for _, f := range record.Plan.Forwards {
			if ports[f.Key()] {
				return r, fmt.Errorf("%w: duplicate mapping in allocation registry", network.ErrPortInUse)
			}
			ports[f.Key()] = true
		}
	}
	return r, nil
}
func validateReservation(r Reservation) error {
	if !state.ValidUUID(r.ID) || !state.ValidUUID(r.OperationID) || !filepath.IsAbs(r.RootPath) {
		return fmt.Errorf("invalid reservation identity")
	}
	return r.Plan.Validate()
}

// CheckIDRange reads current account, process and reservation conflicts.
// Reserve repeats these checks under the allocation lock when a VM is created.
func (c Coordinator) CheckIDRange(ctx context.Context, ids Range) error {
	accounts, err := ReadAccounts()
	if err != nil {
		return err
	}
	if err = addProcessIDs(ctx, &accounts); err != nil {
		return err
	}
	base := c.RuntimeDir
	if base == "" {
		base = "/run/flintyard"
	}
	var used []uint32
	if err = state.CheckPrivateDir(base); err == nil {
		reg, err := readRegistry(filepath.Join(base, "allocations.json"))
		if err != nil {
			return err
		}
		for _, record := range reg.Records {
			for _, node := range record.Plan.Nodes {
				used = append(used, node.UID, node.GID)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	_, err = ChooseIDs(ids, accounts, used, 1)
	return err
}

func (c Coordinator) Reserve(ctx context.Context, r Request) (Reservation, error) {
	accounts, err := ReadAccounts()
	if err != nil {
		return Reservation{}, err
	}
	// /proc status catches dynamic service IDs not represented in passwd/group.
	if err = addProcessIDs(ctx, &accounts); err != nil {
		return Reservation{}, err
	}
	path, lock, err := c.paths()
	if err != nil {
		return Reservation{}, err
	}
	unlock, err := state.LockContext(ctx, lock)
	if err != nil {
		return Reservation{}, err
	}
	defer unlock()
	snapshot, err := network.ReadSnapshot(ctx)
	if err != nil {
		return Reservation{}, err
	}
	return reserveLocked(ctx, path, r, snapshot, accounts)
}
func reserveLocked(ctx context.Context, path string, r Request, snapshot network.Snapshot, accounts Accounts) (Reservation, error) {
	if err := ctx.Err(); err != nil {
		return Reservation{}, err
	}
	if !state.ValidUUID(r.Root.ID) || r.Network.RootID != r.Root.ID || !state.ValidUUID(r.OperationID) {
		return Reservation{}, fmt.Errorf("reservation root or operation identity mismatch")
	}
	actual, err := state.OpenRoot(ctx, r.Root.Path, false)
	if err != nil || actual.ID != r.Root.ID {
		return Reservation{}, fmt.Errorf("state root identity changed: %w", err)
	}
	reg, err := readRegistry(path)
	if err != nil {
		return Reservation{}, err
	}
	// Restore only explicitly retained environments from this verified root.
	// Never discard a stopped environment because /run was lost on reboot.
	for _, retained := range r.Retained {
		if err = validateReservation(retained); err != nil {
			return Reservation{}, err
		}
		if retained.Plan.RootID != r.Root.ID || retained.RootPath != r.Root.Path {
			return Reservation{}, fmt.Errorf("retained reservation owner mismatch")
		}
		found := false
		for _, existing := range reg.Records {
			if existing.ID == retained.ID {
				if !reflect.DeepEqual(existing, retained) {
					return Reservation{}, fmt.Errorf("retained reservation changed")
				}
				found = true
			} else if existing.Plan.RootID == retained.Plan.RootID && existing.Plan.EnvironmentID == retained.Plan.EnvironmentID {
				return Reservation{}, fmt.Errorf("retained environment has another reservation identity")
			} else if existing.Plan.CIDR.Overlaps(retained.Plan.CIDR) {
				return Reservation{}, fmt.Errorf("retained subnet conflicts with an existing reservation")
			} else if network.MappingConflict(existing.Plan.Forwards, retained.Plan.Forwards) {
				return Reservation{}, fmt.Errorf("%w: retained mapping conflicts with an existing reservation", network.ErrPortInUse)
			}
		}
		if !found {
			reg.Records = append(reg.Records, retained)
		}
	}
	request := r.Network
	request.Occupied = append(request.Occupied, snapshot.Occupied...)
	for _, link := range snapshot.Links {
		request.ExistingNames = append(request.ExistingNames, link.Name)
		if link.MAC != "" {
			request.ExistingMACs = append(request.ExistingMACs, link.MAC)
		}
	}
	var used []uint32
	for _, existing := range reg.Records {
		if existing.RootPath == r.Root.Path && existing.Plan.RootID != r.Root.ID {
			return Reservation{}, fmt.Errorf("registered path belongs to another state root identity")
		}
		if existing.Plan.RootID == r.Root.ID && existing.RootPath != r.Root.Path {
			return Reservation{}, fmt.Errorf("same state root identity registered at another path")
		}
		if existing.Plan.RootID == r.Root.ID && existing.Plan.EnvironmentID == r.Network.EnvironmentID {
			return Reservation{}, fmt.Errorf("environment already reserved; verify and reuse its reservation")
		}
		request.Occupied = append(request.Occupied, existing.Plan.CIDR)
		request.ExistingNames = append(request.ExistingNames, existing.Plan.HostLink, existing.Plan.PeerLink, existing.Plan.Bridge)
		for _, n := range existing.Plan.Nodes {
			used = append(used, n.UID, n.GID)
			request.ExistingMACs = append(request.ExistingMACs, n.MAC)
			request.ExistingNames = append(request.ExistingNames, n.Tap)
		}
	}
	ids, err := ChooseIDs(r.UIDRange, accounts, used, len(request.Nodes))
	if err != nil {
		return Reservation{}, err
	}
	for i := range request.Nodes {
		request.Nodes[i].UID, request.Nodes[i].GID = ids[i], ids[i]
	}
	plan, err := network.Allocate(request)
	if err != nil {
		return Reservation{}, err
	}
	plan, err = network.ResolveHostConnectivity(ctx, plan, request.Connectivity, snapshot)
	if err != nil {
		return Reservation{}, err
	}
	for _, existing := range reg.Records {
		if network.MappingConflict(plan.Forwards, existing.Plan.Forwards) {
			return Reservation{}, fmt.Errorf("%w: mapping is reserved by another environment", network.ErrPortInUse)
		}
		if plan.NAT && plan.EgressInterface == existing.Plan.HostLink {
			return Reservation{}, fmt.Errorf("NAT egress is another environment's interface")
		}
	}
	reservationID := r.ReservationID
	if reservationID == "" {
		reservationID = state.UUID()
	}
	if !state.ValidUUID(reservationID) {
		return Reservation{}, fmt.Errorf("invalid reservation ID")
	}
	for _, existing := range reg.Records {
		if existing.ID == reservationID {
			return Reservation{}, fmt.Errorf("reservation ID already exists")
		}
	}
	record := Reservation{ID: reservationID, RootPath: r.Root.Path, OperationID: r.OperationID, Plan: plan}
	reg.Records = append(reg.Records, record)
	if err = state.Write(path, reg); err != nil {
		return Reservation{}, err
	}
	return record, nil
}
func (c Coordinator) Release(ctx context.Context, want Reservation) error {
	if err := validateReservation(want); err != nil {
		return err
	}
	path, lock, err := c.paths()
	if err != nil {
		return err
	}
	unlock, err := state.LockContext(ctx, lock)
	if err != nil {
		return err
	}
	defer unlock()
	reg, err := readRegistry(path)
	if err != nil {
		return err
	}
	for i, record := range reg.Records {
		if record.ID == want.ID {
			if !reflect.DeepEqual(record, want) {
				return fmt.Errorf("reservation ownership changed")
			}
			reg.Records = append(reg.Records[:i], reg.Records[i+1:]...)
			return state.Write(path, reg)
		}
	}
	return nil
}
func addProcessIDs(ctx context.Context, a *Accounts) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		f, err := os.Open(filepath.Join("/proc", entry.Name(), "status"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			fields := strings.Fields(s.Text())
			if len(fields) == 0 || fields[0] != "Uid:" && fields[0] != "Gid:" {
				continue
			}
			target := a.UIDs
			if fields[0] == "Gid:" {
				target = a.GIDs
			}
			if len(fields) != 5 {
				f.Close()
				return fmt.Errorf("invalid process credentials")
			}
			for _, v := range fields[1:] {
				id, e := strconv.ParseUint(v, 10, 32)
				if e != nil {
					f.Close()
					return e
				}
				target[uint32(id)] = true
			}
		}
		err = s.Err()
		f.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (c Coordinator) Reservations(ctx context.Context) ([]Reservation, error) {
	path, lock, err := c.paths()
	if err != nil {
		return nil, err
	}
	unlock, err := state.LockContext(ctx, lock)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := readRegistry(path)
	return r.Records, err
}

// Existing reads the atomic registry without creating runtime directories or
// lock files. It is used only to fail closed when durable state was lost.
func (c Coordinator) Existing(ctx context.Context) ([]Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base := c.RuntimeDir
	if base == "" {
		base = "/run/flintyard"
	}
	if err := state.CheckPrivateDir(base); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	r, err := readRegistry(filepath.Join(base, "allocations.json"))
	return r.Records, err
}
