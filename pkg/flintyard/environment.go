package flintyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LeeXN/Flintyard/internal/allocation"
	"github.com/LeeXN/Flintyard/internal/contract"
	"github.com/LeeXN/Flintyard/internal/firecracker"
	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/network"
	"github.com/LeeXN/Flintyard/internal/state"
)

type EnvironmentSpec struct {
	APIVersion   string        `json:"api_version"`
	Kind         string        `json:"kind"`
	Name         string        `json:"name"`
	Architecture string        `json:"architecture"`
	Network      NetworkSpec   `json:"network"`
	Nodes        []NodeSpec    `json:"nodes"`
	Access       AccessSpec    `json:"access"`
	PortForwards []PortForward `json:"port_forwards"`
}
type NetworkSpec struct {
	CIDR            string   `json:"cidr"`
	NAT             bool     `json:"nat"`
	DNS             []string `json:"dns"`
	EgressInterface string   `json:"egress_interface,omitempty"`
}
type NodeSpec struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	VCPUs     int    `json:"vcpu_count"`
	MemoryMiB int    `json:"memory_mib"`
	IPv4      string `json:"ipv4,omitempty"`
}
type AccessSpec struct {
	PublicKey string      `json:"public_key,omitempty"`
	Probe     image.Probe `json:"probe"`
}
type PortForward = network.ForwardRequest

type NodeState struct {
	ID                 string                `json:"id"`
	Name               string                `json:"name"`
	State              string                `json:"state"`
	ImageID            string                `json:"image_id"`
	Username           string                `json:"username"`
	Disk               image.Disk            `json:"disk"`
	Seed               string                `json:"seed,omitempty"`
	Jail               *firecracker.Jail     `json:"jail,omitempty"`
	Cgroup             *firecracker.Cgroup   `json:"cgroup,omitempty"`
	Process            *firecracker.Identity `json:"process,omitempty"`
	FirecrackerVersion string                `json:"firecracker_version,omitempty"`
	LastProbeAt        *time.Time            `json:"last_probe_at,omitempty"`
	LastProbePassed    bool                  `json:"last_probe_passed"`
	PendingLaunch      bool                  `json:"-"`
}
type environmentState struct {
	APIVersion      string                  `json:"api_version"`
	Kind            string                  `json:"kind"`
	RootID          string                  `json:"root_id"`
	ID              string                  `json:"id"`
	Name            string                  `json:"name"`
	State           string                  `json:"state"`
	Architecture    string                  `json:"architecture"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
	Digest          string                  `json:"resolved_digest"`
	Spec            EnvironmentSpec         `json:"spec"`
	PublicKeySHA256 string                  `json:"public_key_sha256,omitempty"`
	PublicKeys      string                  `json:"public_keys,omitempty"`
	Reservation     *allocation.Reservation `json:"reservation,omitempty"`
	Nodes           []NodeState             `json:"nodes"`
	Operations      []string                `json:"operations"`
	KeepFailed      bool                    `json:"keep_failed"`
	Recovered       bool                    `json:"-"`
}
type NodeAccess struct {
	Username      string `json:"username"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Probe         string `json:"probe"`
	ProbePassed   bool   `json:"probe_passed"`
	Authenticated bool   `json:"authenticated"`
}
type NodeResult struct {
	ID                 string     `json:"node_id"`
	Name               string     `json:"name"`
	Hostname           string     `json:"hostname"`
	Architecture       string     `json:"architecture"`
	State              string     `json:"state"`
	PersistedState     string     `json:"persisted_state"`
	IPv4               string     `json:"ipv4"`
	MAC                string     `json:"mac"`
	Access             NodeAccess `json:"access"`
	Stale              bool       `json:"stale"`
	LastProbeAt        *time.Time `json:"last_probe_at,omitempty"`
	LastProbePassed    bool       `json:"last_probe_passed"`
	ObservationError   *Error     `json:"observation_error,omitempty"`
	FirecrackerVersion string     `json:"firecracker_version,omitempty"`
}
type EnvironmentResult struct {
	EnvironmentID      string         `json:"environment_id"`
	Name               string         `json:"name"`
	Architecture       string         `json:"architecture"`
	State              string         `json:"state"`
	PersistedState     string         `json:"persisted_state"`
	ObservedAt         time.Time      `json:"observed_at"`
	Nodes              []NodeResult   `json:"nodes"`
	Network            *NetworkResult `json:"network,omitempty"`
	RecoverySource     string         `json:"recovery_source,omitempty"`
	DiskAvailableBytes *uint64        `json:"disk_available_bytes,omitempty"`
	Warnings           []string       `json:"warnings,omitempty"`
}
type ForwardResult struct {
	Protocol         string `json:"protocol"`
	BindAddress      string `json:"bind_address"`
	HostPort         int    `json:"host_port"`
	Node             string `json:"node"`
	GuestPort        int    `json:"guest_port"`
	HostEndpoint     string `json:"host_endpoint"`
	ExternalEndpoint string `json:"external_endpoint,omitempty"`
	ExternalVerified bool   `json:"external_verified"`
}
type NetworkResult struct {
	NAT             bool            `json:"nat"`
	EgressInterface string          `json:"egress_interface,omitempty"`
	Forwards        []ForwardResult `json:"port_forwards"`
	Diagnostics     []string        `json:"diagnostics"`
}
type StatusRequest struct {
	Environment string
	Probe       bool
}
type ListRequest struct{ Probe bool }
type ListResult struct {
	Environments []EnvironmentResult `json:"environments"`
}
type StopRequest struct{ Environment string }
type DownRequest struct {
	Environment string
	Purge       bool
}
type CleanupResult struct {
	EnvironmentID     string   `json:"environment_id"`
	Name              string   `json:"name"`
	State             string   `json:"state"`
	AlreadyAbsent     bool     `json:"already_absent"`
	Errors            []string `json:"cleanup_errors"`
	DiagnosticsPurged bool     `json:"diagnostics_purged"`
}
type UpRequest struct {
	File, Wait, Firecracker, Jailer, UIDRange string
	KeepFailed                                bool
	Concurrency                               int
}

var environmentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
var errEnvironmentNotFound = errors.New("environment not found")

func envDir(root state.Root, id string) string { return filepath.Join(root.Path, "envs", id) }
func nodeDir(root state.Root, env, node string) string {
	return filepath.Join(envDir(root, env), "nodes", node)
}
func loadEnvironment(root state.Root, id string) (environmentState, error) {
	var e environmentState
	if !state.ValidUUID(id) {
		return e, fmt.Errorf("invalid environment ID")
	}
	if err := state.CheckPrivateDir(envDir(root, id)); err != nil {
		return e, err
	}
	if err := state.VerifyOwner(envDir(root, id), state.Owner{RootID: root.ID, EnvironmentID: id}); err != nil {
		return e, err
	}
	if err := state.Read(filepath.Join(envDir(root, id), "state.json"), &e); err != nil {
		if !os.IsNotExist(err) {
			return e, err
		}
		var recoveryErr error
		e, recoveryErr = readCheckpoint(root, id)
		if recoveryErr != nil {
			return e, fmt.Errorf("primary environment state missing; recovery unavailable: %w", recoveryErr)
		}
		e.Recovered = true
	}
	if e.RootID != root.ID || e.ID != id || e.APIVersion != APIVersion || e.Kind != "EnvironmentState" || !environmentName.MatchString(e.Name) || e.Spec.Name != e.Name || e.Spec.Architecture != e.Architecture {
		return e, fmt.Errorf("environment state ownership/configuration mismatch")
	}
	validPhase := func(phase string) bool {
		switch phase {
		case "preparing", "starting", "running", "ready", "stopped", "failed", "deleting", "deleted":
			return true
		}
		return false
	}
	if !validPhase(e.State) || len(e.Nodes) < 1 || len(e.Nodes) > 128 || len(e.Spec.Nodes) != len(e.Nodes) {
		return e, fmt.Errorf("invalid environment phase or node inventory")
	}
	configured := map[string]NodeSpec{}
	for _, n := range e.Spec.Nodes {
		if _, exists := configured[n.Name]; exists {
			return e, fmt.Errorf("duplicate persisted node specification")
		}
		configured[n.Name] = n
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, n := range e.Nodes {
		if !state.ValidUUID(n.ID) || ids[n.ID] || names[n.Name] || !environmentName.MatchString(n.Name) {
			return e, fmt.Errorf("invalid persisted node identity")
		}
		if spec, ok := configured[n.Name]; !ok || spec.Image != n.ImageID || !validPhase(n.State) {
			return e, fmt.Errorf("persisted node/specification mismatch")
		}
		ids[n.ID], names[n.Name] = true, true
	}
	if e.Reservation != nil && (e.Reservation.Plan.RootID != root.ID || e.Reservation.Plan.EnvironmentID != id) {
		return e, fmt.Errorf("environment reservation owner mismatch")
	}
	if e.Reservation != nil {
		if err := e.Reservation.Plan.Validate(); err != nil {
			return e, err
		}
		p := e.Reservation.Plan
		if p.NAT != e.Spec.Network.NAT || p.EgressInterface != e.Spec.Network.EgressInterface || len(p.Forwards) != len(e.Spec.PortForwards) {
			return e, fmt.Errorf("reservation connectivity differs from persisted specification")
		}
		for i, f := range p.Forwards {
			want := e.Spec.PortForwards[i]
			if f.Protocol != want.Protocol || f.BindAddress.String() != want.BindAddress || f.HostPort != want.HostPort || f.Node != want.Node || f.GuestPort != want.GuestPort {
				return e, fmt.Errorf("reservation mapping differs from persisted specification")
			}
		}
		if len(e.Reservation.Plan.Nodes) != len(e.Nodes) {
			return e, fmt.Errorf("reservation node inventory changed")
		}
		for _, node := range e.Reservation.Plan.Nodes {
			if !ids[node.ID] || !names[node.Name] {
				return e, fmt.Errorf("reservation references another node")
			}
		}
	}
	return e, nil
}
func scanEnvironments(ctx context.Context, root state.Root) ([]environmentState, error) {
	entries, err := os.ReadDir(filepath.Join(root.Path, "envs"))
	if os.IsNotExist(err) {
		return []environmentState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var result []environmentState
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !state.ValidUUID(entry.Name()) {
			continue
		} // Bootstrap staging is not an environment identity.
		if !entry.IsDir() {
			return nil, fmt.Errorf("non-directory environment entry")
		}
		e, err := loadEnvironment(root, entry.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}
func findEnvironment(ctx context.Context, root state.Root, query string) (environmentState, error) {
	if state.ValidUUID(query) {
		if _, err := os.Lstat(envDir(root, query)); os.IsNotExist(err) {
			return environmentState{}, errEnvironmentNotFound
		} else if err != nil {
			return environmentState{}, err
		}
		return loadEnvironment(root, query)
	}
	if !environmentName.MatchString(query) {
		return environmentState{}, fmt.Errorf("invalid environment name or UUID")
	}
	all, err := scanEnvironments(ctx, root)
	if err != nil {
		return environmentState{}, err
	}
	var found *environmentState
	for i := range all {
		e := &all[i]
		if e.Name != query {
			continue
		}
		if found != nil && found.State != "deleted" && e.State != "deleted" {
			return environmentState{}, fmt.Errorf("multiple active environments share a name")
		}
		if found == nil || found.State == "deleted" {
			found = e
		}
	}
	if found == nil {
		return environmentState{}, errEnvironmentNotFound
	}
	return *found, nil
}
func saveEnvironment(root state.Root, e *environmentState) error {
	if e.RootID != root.ID || !state.ValidUUID(e.ID) {
		return fmt.Errorf("cannot write environment with another owner")
	}
	if err := state.VerifyOwner(envDir(root, e.ID), state.Owner{RootID: root.ID, EnvironmentID: e.ID}); err != nil {
		return err
	}
	e.UpdatedAt = time.Now().UTC()
	checkpoint, err := makeCheckpoint(*e)
	if err != nil {
		return err
	}
	if err = state.Write(filepath.Join(envDir(root, e.ID), "recovery.json"), checkpoint); err != nil {
		return err
	}
	return state.Write(filepath.Join(envDir(root, e.ID), "state.json"), e)
}
func lockEnvironment(ctx context.Context, root state.Root, name string) (func(), error) {
	if !environmentName.MatchString(name) {
		return nil, fmt.Errorf("invalid environment name")
	}
	if err := state.CheckPrivateDir(filepath.Join(root.Path, "locks")); err != nil {
		return nil, err
	}
	return state.LockContext(ctx, filepath.Join(root.Path, "locks", name+".lock"))
}
func resultFromState(e environmentState) EnvironmentResult {
	r := EnvironmentResult{EnvironmentID: e.ID, Name: e.Name, Architecture: e.Architecture, State: e.State, PersistedState: e.State, ObservedAt: time.Now().UTC(), Nodes: []NodeResult{}}
	if e.Recovered {
		r.RecoverySource = "checkpoint"
	}
	for _, node := range e.Nodes {
		n := NodeResult{ID: node.ID, Name: node.Name, Architecture: e.Architecture, State: node.State, PersistedState: node.State, Stale: true, LastProbeAt: node.LastProbeAt, LastProbePassed: node.LastProbePassed, Access: NodeAccess{Username: node.Username, Port: e.Spec.Access.Probe.Port, Probe: e.Spec.Access.Probe.Type}}
		n.FirecrackerVersion = node.FirecrackerVersion
		if e.Reservation != nil {
			for _, p := range e.Reservation.Plan.Nodes {
				if p.ID == node.ID {
					n.IPv4, n.Hostname, n.MAC = p.IPv4.String(), p.Hostname, p.MAC
					n.Access.Host = n.IPv4
				}
			}
		}
		r.Nodes = append(r.Nodes, n)
	}
	if e.Reservation != nil {
		r.Network = networkResult(e.Reservation.Plan)
	}
	return r
}
func canonicalSpec(spec EnvironmentSpec, keySHA string) (string, error) {
	spec.Access.PublicKey = ""
	spec.Nodes = append([]NodeSpec(nil), spec.Nodes...)
	sort.Slice(spec.Nodes, func(i, j int) bool { return spec.Nodes[i].Name < spec.Nodes[j].Name })
	b, err := json.Marshal(struct {
		Spec EnvironmentSpec `json:"spec"`
		Key  string          `json:"key"`
	}{spec, keySHA})
	if err != nil {
		return "", err
	}
	return image.Sum(b), nil
}

func hydrateProcesses(root state.Root, e *environmentState) error {
	entries, err := os.ReadDir(filepath.Join(envDir(root, e.ID), "operations"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	active := map[string]bool{}
	for _, entry := range entries {
		op := strings.TrimSuffix(entry.Name(), ".json")
		if !state.ValidUUID(op) || entry.Name() != op+".json" {
			continue
		}
		j, err := state.ReadJournal(filepath.Join(envDir(root, e.ID), "operations", entry.Name()), e.ID)
		if err != nil {
			return err
		}
		if j.OperationID != op {
			return fmt.Errorf("journal operation identity mismatch")
		}
		for _, resource := range j.Resources {
			if resource.Kind != "vmm-process" || resource.State == "released" {
				continue
			}
			nodeID := resource.Identity["node_id"]
			if active[nodeID] {
				return fmt.Errorf("multiple active launcher records for a node")
			}
			active[nodeID] = true
			found := false
			for i := range e.Nodes {
				n := &e.Nodes[i]
				if n.ID != nodeID {
					continue
				}
				found = true
				if text := resource.Identity["identity"]; text != "" {
					var identity firecracker.Identity
					if err := contract.Decode([]byte(text), &identity); err != nil {
						return err
					}
					n.Process = &identity
					if n.Jail == nil {
						run := resource.Identity["run_dir"]
						n.Jail = &firecracker.Jail{RunDir: run, Root: resource.Identity["jail_root"], Socket: filepath.Join(run, "api", "api.sock")}
					}
				} else {
					n.Process = nil
					n.PendingLaunch = true
				}
			}
			if !found {
				return fmt.Errorf("launcher record references unknown node")
			}
		}
	}
	return nil
}
