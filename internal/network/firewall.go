package network

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/LeeXN/Flintyard/internal/host"
)

type object map[string]any

func meta(key string) object { return object{"meta": object{"key": key}} }
func payload(proto, field string) object {
	return object{"payload": object{"protocol": proto, "field": field}}
}
func match(left, right any) object {
	return object{"match": object{"op": "==", "left": left, "right": right}}
}

// PrivateFirewall is one owned create transaction. Authorizations bind the
// interfaces, environment addresses, direction and original conntrack tuple;
// established state alone never bypasses private isolation.
func PrivateFirewall(p Plan, namespace bool) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	family := "inet"
	if namespace {
		family = "bridge"
	}
	commands := []any{object{"create": object{"table": object{"family": family, "name": p.Table}}}}
	baseChain := func(name, hook, typ string, priority int) {
		c := object{"family": family, "table": p.Table, "name": name}
		if hook != "" {
			c["type"], c["hook"], c["prio"], c["policy"] = typ, hook, priority, "accept"
		}
		commands = append(commands, object{"add": object{"chain": c}})
	}
	chain := func(name, hook string) { baseChain(name, hook, "filter", 0) }
	rule := func(name, comment string, expr ...any) {
		commands = append(commands, object{"add": object{"rule": object{"family": family, "table": p.Table, "chain": name, "comment": comment, "expr": expr}}})
	}
	chain("owner", "")
	// nft 1.0.x JSON does not expose table comments. An inert owner chain and
	// exact rule comment mark the whole atomically created table instead.
	rule("owner", p.Owner, object{"counter": nil})
	chain("forward", "forward")
	if namespace {
		rule("forward", "unmanaged IPv6", match(payload("ether", "type"), "ip6"), object{"drop": nil})
		chain("input", "input")
		rule("input", "unmanaged IPv6", match(payload("ether", "type"), "ip6"), object{"drop": nil})
	} else {
		prefix := object{"prefix": object{"addr": p.CIDR.Addr().String(), "len": p.CIDR.Bits()}}
		ct := func(key string) object { return object{"ct": object{"key": key}} }
		original := func(key string, ip bool) object {
			v := object{"key": key, "dir": "original"}
			if ip {
				v["family"] = "ip"
			}
			return object{"ct": v}
		}
		stateAllowed := func(mask int) object {
			return object{"match": object{"op": "!=", "left": object{"&": []any{ct("state"), mask}}, "right": 0}}
		}
		mappingTuple := func(f Forward) []any {
			return []any{
				match(meta("nfproto"), "ipv4"),
				match(object{"&": []any{ct("status"), 32}}, 32),
				match(original("protocol", false), f.Protocol), match(original("daddr", true), f.BindAddress.String()), match(original("proto-dst", false), f.HostPort),
			}
		}
		for _, f := range p.Forwards {
			if f.BindAddress.IsLoopback() {
				continue
			} // OUTPUT mappings never authorize forwarded 127/8 packets.
			in := []any{match(meta("oifname"), p.HostLink), match(payload("ip", "daddr"), f.GuestIP.String()), match(ct("direction"), "original"), stateAllowed(8 | 2 | 4)}
			in = append(in, mappingTuple(f)...)
			in = append(in, object{"accept": nil})
			rule("forward", "mapped ingress", in...)
			out := []any{match(meta("iifname"), p.HostLink), match(payload("ip", "saddr"), f.GuestIP.String()), match(ct("direction"), "reply"), stateAllowed(2 | 4)}
			out = append(out, mappingTuple(f)...)
			out = append(out, object{"accept": nil})
			rule("forward", "mapped reply", out...)
		}
		if p.NAT {
			rule("forward", "NAT egress", match(meta("iifname"), p.HostLink), match(meta("oifname"), p.EgressInterface), match(payload("ip", "saddr"), prefix), match(original("saddr", true), prefix), match(ct("direction"), "original"), stateAllowed(8|2|4), object{"accept": nil})
			rule("forward", "NAT reply", match(meta("iifname"), p.EgressInterface), match(meta("oifname"), p.HostLink), match(payload("ip", "daddr"), prefix), match(original("saddr", true), prefix), match(ct("direction"), "reply"), stateAllowed(2|4), object{"accept": nil})
		}
		rule("forward", "private egress", match(meta("iifname"), p.HostLink), object{"drop": nil})
		rule("forward", "private ingress", match(meta("oifname"), p.HostLink), object{"drop": nil})
		chain("input", "input")
		rule("input", "unmanaged IPv6", match(meta("iifname"), p.HostLink), match(meta("nfproto"), "ipv6"), object{"drop": nil})
		chain("output", "output")
		rule("output", "unmanaged IPv6", match(meta("oifname"), p.HostLink), match(meta("nfproto"), "ipv6"), object{"drop": nil})
		if p.NAT || len(p.Forwards) > 0 {
			baseChain("prerouting", "prerouting", "nat", -100)
			baseChain("nat_output", "output", "nat", -100)
			baseChain("postrouting", "postrouting", "nat", 100)
			for _, f := range p.Forwards {
				dnat := object{"dnat": object{"family": "ip", "addr": f.GuestIP.String(), "port": f.GuestPort}}
				expr := []any{match(payload("ip", "daddr"), f.BindAddress.String()), match(meta("l4proto"), f.Protocol), match(payload(f.Protocol, "dport"), f.HostPort), dnat}
				rule("nat_output", "local mapping", expr...)
				if !f.BindAddress.IsLoopback() {
					external := []any{object{"match": object{"op": "!=", "left": meta("iifname"), "right": p.HostLink}}, object{"match": object{"op": "!=", "left": payload("ip", "saddr"), "right": object{"prefix": object{"addr": "127.0.0.0", "len": 8}}}}}
					external = append(external, expr...)
					rule("prerouting", "external mapping", external...)
				}
				snat := []any{match(meta("oifname"), p.HostLink), match(payload("ip", "daddr"), f.GuestIP.String()), match(ct("direction"), "original")}
				snat = append(snat, mappingTuple(f)...)
				snat = append(snat, object{"snat": object{"family": "ip", "addr": p.Gateway.String()}})
				rule("postrouting", "mapping return path", snat...)
			}
			if p.NAT {
				rule("postrouting", "environment masquerade", match(meta("oifname"), p.EgressInterface), match(payload("ip", "saddr"), prefix), object{"masquerade": nil})
			}
		}
	}
	return json.Marshal(object{"nftables": commands})
}

type nftListing struct {
	Items []struct {
		Table *struct {
			Family string `json:"family"`
			Name   string `json:"name"`
			Handle uint64 `json:"handle"`
		} `json:"table"`
		Rule *struct {
			Family  string `json:"family"`
			Table   string `json:"table"`
			Chain   string `json:"chain"`
			Comment string `json:"comment"`
		} `json:"rule"`
	} `json:"nftables"`
}

func nftRequest(ns string, args []string, input []byte) host.CommandRequest {
	r := host.CommandRequest{Name: "nft", Args: args, Input: bytes.NewReader(input)}
	if ns != "" {
		r.Name = "ip"
		r.Args = append([]string{"netns", "exec", ns, "nft"}, args...)
	}
	return r
}
func firewallHandle(ctx context.Context, p Plan, ns string) (uint64, error) {
	family := "inet"
	if ns != "" {
		family = "bridge"
	}
	var listing nftListing
	if err := host.RunJSON(ctx, nftRequest(ns, []string{"-j", "list", "tables"}, nil), &listing); err != nil {
		return 0, err
	}
	exists := false
	for _, item := range listing.Items {
		if item.Table != nil && item.Table.Name == p.Table && item.Table.Family == family {
			exists = true
		}
	}
	if !exists {
		return 0, nil
	}
	if err := host.RunJSON(ctx, nftRequest(ns, []string{"-j", "list", "table", family, p.Table}, nil), &listing); err != nil {
		return 0, err
	}
	var handle uint64
	owned := false
	for _, item := range listing.Items {
		if item.Table != nil && item.Table.Name == p.Table && item.Table.Family == family {
			handle = item.Table.Handle
		}
		if item.Rule != nil && item.Rule.Family == family && item.Rule.Table == p.Table && item.Rule.Chain == "owner" && item.Rule.Comment == p.Owner {
			owned = true
		}
	}
	if !owned || handle == 0 {
		return 0, fmt.Errorf("%w: nft table marker missing or changed", ErrOwnership)
	}
	return handle, nil
}
