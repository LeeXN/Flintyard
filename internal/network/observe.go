package network

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/LeeXN/Flintyard/internal/host"
)

type Link struct {
	Index     int    `json:"ifindex"`
	Name      string `json:"ifname"`
	Alias     string `json:"ifalias"`
	PeerIndex int    `json:"link_index"`
	PeerName  string `json:"link"`
	MAC       string `json:"address"`
	Info      struct {
		Kind string `json:"info_kind"`
		Data struct {
			Type  string `json:"type"`
			User  uint32 `json:"user"`
			Group uint32 `json:"group"`
		} `json:"info_data"`
	} `json:"linkinfo"`
	Addresses []struct {
		Family string `json:"family"`
		Local  string `json:"local"`
		Prefix int    `json:"prefixlen"`
	} `json:"addr_info"`
}
type Snapshot struct {
	Occupied      []netip.Prefix
	Links         []Link
	DefaultEgress []string
}

func ReadSnapshot(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	if err := host.RunJSON(ctx, host.CommandRequest{Name: "ip", Args: []string{"-j", "-d", "-4", "address", "show"}}, &s.Links); err != nil {
		return s, err
	}
	for _, link := range s.Links {
		for _, v := range link.Addresses {
			if v.Family != "inet" {
				continue
			}
			a, err := netip.ParseAddr(v.Local)
			if err != nil || !a.Is4() || v.Prefix < 0 || v.Prefix > 32 {
				return s, fmt.Errorf("invalid host address snapshot")
			}
			s.Occupied = append(s.Occupied, netip.PrefixFrom(a, v.Prefix).Masked())
		}
	}
	var routes []struct {
		Dst       string `json:"dst"`
		Dev       string `json:"dev"`
		Multipath []struct {
			Dev string `json:"dev"`
		} `json:"multipath"`
	}
	if err := host.RunJSON(ctx, host.CommandRequest{Name: "ip", Args: []string{"-j", "-4", "route", "show", "table", "all"}}, &routes); err != nil {
		return s, err
	}
	for _, r := range routes {
		if r.Dst == "default" {
			if len(r.Multipath) > 0 {
				for _, hop := range r.Multipath {
					s.DefaultEgress = append(s.DefaultEgress, hop.Dev)
				}
			} else {
				s.DefaultEgress = append(s.DefaultEgress, r.Dev)
			}
			continue
		}
		p, err := parsePrefix(r.Dst)
		if err != nil {
			return s, err
		}
		s.Occupied = append(s.Occupied, p)
	}
	var rules []struct {
		From string `json:"src"`
		To   string `json:"dst"`
	}
	if err := host.RunJSON(ctx, host.CommandRequest{Name: "ip", Args: []string{"-j", "-4", "rule", "show"}}, &rules); err != nil {
		return s, err
	}
	for _, r := range rules {
		for _, v := range []string{r.From, r.To} {
			if v == "" || v == "all" || v == "0.0.0.0/0" {
				continue
			}
			p, err := parsePrefix(v)
			if err != nil {
				return s, err
			}
			s.Occupied = append(s.Occupied, p)
		}
	}
	return s, nil
}
func parsePrefix(v string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(v); err == nil && a.Is4() {
		return netip.PrefixFrom(a, 32), nil
	}
	p, err := netip.ParsePrefix(v)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("cannot interpret IPv4 route/rule %q", v)
	}
	return p.Masked(), nil
}
func ipLinks(ctx context.Context, ns string) ([]Link, error) {
	args := []string{"-j", "-d"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	args = append(args, "link", "show")
	var links []Link
	err := host.RunJSON(ctx, host.CommandRequest{Name: "ip", Args: args}, &links)
	return links, err
}
func findLink(ctx context.Context, ns, name string) (*Link, error) {
	links, err := ipLinks(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.Name == name {
			return &l, nil
		}
	}
	return nil, nil
}
