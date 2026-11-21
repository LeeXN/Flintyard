package flintyard

import (
	"context"
	"os"
	"path/filepath"

	"github.com/LeeXN/Flintyard/internal/state"
)

// An unreadable/ownerless environment cannot authorize cleanup, but must not
// hide independently verifiable siblings from a read-only GC inventory.
func scanGCEnvironments(ctx context.Context, root state.Root) ([]environmentState, []GCExcluded, error) {
	var environments []environmentState
	excluded := []GCExcluded{}
	base := filepath.Join(root.Path, "envs")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return environments, excluded, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err = state.CheckPrivateDir(base); err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !state.ValidUUID(entry.Name()) {
			continue
		}
		if !entry.IsDir() {
			excluded = append(excluded, GCExcluded{EnvironmentID: entry.Name(), Reason: "non-directory environment entry; no cleanup authority"})
			continue
		}
		env, err := loadEnvironment(root, entry.Name())
		if err != nil {
			excluded = append(excluded, GCExcluded{EnvironmentID: entry.Name(), Reason: "unverifiable environment: " + err.Error()})
			continue
		}
		environments = append(environments, env)
	}
	return environments, excluded, nil
}
