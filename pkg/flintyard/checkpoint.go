package flintyard

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/LeeXN/Flintyard/internal/image"
	"github.com/LeeXN/Flintyard/internal/state"
)

// The recovery copy is written before the primary state. If a crash interrupts
// commit it may be ahead, which conservatively retains ready/stopped intent;
// it can never lag a successfully committed new primary state.
type environmentCheckpoint struct {
	APIVersion    string           `json:"api_version"`
	Kind          string           `json:"kind"`
	RootID        string           `json:"root_id"`
	EnvironmentID string           `json:"environment_id"`
	Environment   environmentState `json:"environment"`
	SHA256        string           `json:"environment_sha256"`
}

func makeCheckpoint(e environmentState) (environmentCheckpoint, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return environmentCheckpoint{}, err
	}
	return environmentCheckpoint{APIVersion: APIVersion, Kind: "EnvironmentCheckpoint", RootID: e.RootID, EnvironmentID: e.ID, Environment: e, SHA256: image.Sum(b)}, nil
}
func readCheckpoint(root state.Root, id string) (environmentState, error) {
	var checkpoint environmentCheckpoint
	if err := state.Read(filepath.Join(envDir(root, id), "recovery.json"), &checkpoint); err != nil {
		return environmentState{}, err
	}
	b, err := json.Marshal(checkpoint.Environment)
	if err != nil {
		return environmentState{}, err
	}
	if checkpoint.APIVersion != APIVersion || checkpoint.Kind != "EnvironmentCheckpoint" || checkpoint.RootID != root.ID || checkpoint.EnvironmentID != id || checkpoint.Environment.RootID != root.ID || checkpoint.Environment.ID != id || !image.DigestValid(checkpoint.SHA256) || image.Sum(b) != checkpoint.SHA256 {
		return environmentState{}, fmt.Errorf("environment recovery checkpoint identity/digest mismatch")
	}
	return checkpoint.Environment, nil
}
