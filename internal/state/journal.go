package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Journal records intent before creation and observed ownership after creation.
// Its caller must hold an operation lock for the lifetime of the transaction.
type Journal struct {
	APIVersion  string     `json:"api_version"`
	OperationID string     `json:"operation_id"`
	OwnerID     string     `json:"owner_id"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Resources   []Resource `json:"resources"`
	path        string
}
type Resource struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	State    string            `json:"state"`
	Identity map[string]string `json:"identity"`
}

func NewJournal(path, owner string) (*Journal, error) {
	return NewOperationJournal(path, owner, UUID())
}
func NewOperationJournal(path, owner, operation string) (*Journal, error) {
	if !ValidUUID(operation) {
		return nil, fmt.Errorf("invalid operation ID")
	}
	j := &Journal{APIVersion: "flintyard/v1alpha1", OperationID: operation, OwnerID: owner, Resources: []Resource{}, path: path}
	if owner == "" {
		return nil, fmt.Errorf("journal requires an owner")
	}
	if _, e := os.Lstat(path); !os.IsNotExist(e) {
		return nil, fmt.Errorf("journal already exists or is unreadable")
	}
	return j, j.save()
}
func (j *Journal) save() error { j.UpdatedAt = time.Now().UTC(); return Write(j.path, j) }
func (j *Journal) Intent(kind string) (string, error) {
	return j.IntentIdentity(kind, nil)
}

// IntentIdentity durably records the planned owner tag and exact resource name
// before creation. Recovery still requires independent kernel ownership proof.
func (j *Journal) IntentIdentity(kind string, desired map[string]string) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("resource kind required")
	}
	r := Resource{ID: UUID(), Kind: kind, State: "intent", Identity: map[string]string{}}
	for k, v := range desired {
		r.Identity[k] = v
	}
	j.Resources = append(j.Resources, r)
	return r.ID, j.save()
}

// Absent completes an intent only after a subsystem has checked that its exact
// target is absent under its resource lock. It is distinct from releasing an
// observed resource and must never be inferred from an incomplete done record.
func (j *Journal) Absent(id string) error {
	for i := range j.Resources {
		if j.Resources[i].ID == id && j.Resources[i].State == "intent" {
			j.Resources[i].State = "released"
			return j.save()
		}
	}
	return fmt.Errorf("unknown or observed resource cannot be completed as absent")
}
func (j *Journal) Done(id string, identity map[string]string) error {
	if len(identity) == 0 {
		return fmt.Errorf("observed identity is required")
	}
	for i := range j.Resources {
		r := &j.Resources[i]
		if r.ID == id && r.State == "intent" {
			r.Identity = map[string]string{}
			for k, v := range identity {
				r.Identity[k] = v
			}
			r.State = "done"
			return j.save()
		}
	}
	return fmt.Errorf("unknown or already completed intent")
}

// UpdateIntent adds observations while a multi-step creation (such as exec)
// is still in progress. Completion must still pass through Done.
func (j *Journal) UpdateIntent(id string, observed map[string]string) error {
	for i := range j.Resources {
		if j.Resources[i].ID == id && j.Resources[i].State == "intent" {
			for key, value := range observed {
				j.Resources[i].Identity[key] = value
			}
			return j.save()
		}
	}
	return fmt.Errorf("unknown or completed intent")
}
func (j *Journal) Released(id string) error {
	for i := range j.Resources {
		r := &j.Resources[i]
		if r.ID == id {
			if r.State != "done" && r.State != "released" {
				return fmt.Errorf("unobserved intent cannot be marked released")
			}
			r.State = "released"
			return j.save()
		}
	}
	return fmt.Errorf("unknown resource")
}

// Refresh replaces identity only after a subsystem has revalidated an owned
// object's authorized transition, for example jailer execing its VMM.
func (j *Journal) Refresh(id string, identity map[string]string) error {
	for i := range j.Resources {
		if j.Resources[i].ID == id && j.Resources[i].State == "done" {
			j.Resources[i].Identity = map[string]string{}
			for k, v := range identity {
				j.Resources[i].Identity[k] = v
			}
			return j.save()
		}
	}
	return fmt.Errorf("unknown or inactive resource")
}
func ReadJournal(path, owner string) (*Journal, error) {
	var j Journal
	if e := Read(path, &j); e != nil {
		return nil, e
	}
	if j.OwnerID != owner || j.APIVersion != "flintyard/v1alpha1" || !ValidUUID(j.OperationID) {
		return nil, fmt.Errorf("journal ownership mismatch")
	}
	seen := map[string]bool{}
	for _, resource := range j.Resources {
		if !ValidUUID(resource.ID) || seen[resource.ID] || resource.Kind == "" || resource.State != "intent" && resource.State != "done" && resource.State != "released" || resource.Identity == nil {
			return nil, fmt.Errorf("invalid journal resource identity or state")
		}
		seen[resource.ID] = true
	}
	j.path = filepath.Clean(path)
	return &j, nil
}

// GCPlan is deliberately observational: destruction requires each subsystem to
// recheck its kernel ownership evidence under locks before executing any action.
type GCPlan struct {
	APIVersion string     `json:"api_version"`
	OwnerID    string     `json:"owner_id"`
	BootID     string     `json:"boot_id"`
	ExpiresAt  time.Time  `json:"expires_at"`
	Resources  []Resource `json:"resources"`
	SHA256     string     `json:"sha256"`
}

func (p GCPlan) Canonical() ([]byte, error) { p.SHA256 = ""; return json.Marshal(p) }
