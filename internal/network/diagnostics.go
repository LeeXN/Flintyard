package network

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/LeeXN/Flintyard/internal/host"
)

// PolicyDiagnostics names foreign policy that may block access. It is advisory:
// an accept in this tool's chain cannot override another base chain's drop.
// Conditions and jumps are not interpreted as proof of external reachability.
func PolicyDiagnostics(ctx context.Context, p Plan) ([]string, error) {
	var listing struct {
		Items []struct {
			Chain *struct{ Family, Table, Name, Hook, Policy string } `json:"chain"`
			Rule  *struct {
				Family, Table, Chain string
				Handle               uint64
				Expr                 []map[string]json.RawMessage
			} `json:"rule"`
		} `json:"nftables"`
	}
	if err := host.RunJSON(ctx, nftRequest("", []string{"-j", "list", "ruleset"}, nil), &listing); err != nil {
		return nil, err
	}
	var warnings []string
	for _, item := range listing.Items {
		if c := item.Chain; c != nil && (c.Family == "inet" || c.Family == "ip") && c.Table != p.Table && c.Hook != "" && c.Policy == "drop" {
			warnings = append(warnings, fmt.Sprintf("host nft %s/%s/%s hook %s has drop policy; it can block access despite this environment's allow rules", c.Family, c.Table, c.Name, c.Hook))
		}
		if r := item.Rule; r != nil && (r.Family == "inet" || r.Family == "ip") && r.Table != p.Table {
			for _, expr := range r.Expr {
				if _, ok := expr["drop"]; ok {
					warnings = append(warnings, fmt.Sprintf("host nft %s/%s/%s rule %d contains drop; its conditions may block access", r.Family, r.Table, r.Chain, r.Handle))
					break
				}
				if _, ok := expr["reject"]; ok {
					warnings = append(warnings, fmt.Sprintf("host nft %s/%s/%s rule %d contains reject; its conditions may block access", r.Family, r.Table, r.Chain, r.Handle))
					break
				}
			}
		}
		if len(warnings) >= 64 {
			warnings = append(warnings, "additional host nft policies omitted")
			break
		}
	}
	return warnings, nil
}
