package model

import "fmt"

// BuildIdentities computes the stable per-entry identity used to match rows
// between two routers, per project.md §5.3: the entry's comment if it has
// one, otherwise a per-section fallback (chain + ordinal for firewall
// rules, list+address for firewall address-list entries since they have no
// name field, address for DNS static, name for most other sections, and
// finally a bare ordinal if nothing else identifies the row).
func BuildIdentities(section string, entries []Entry) []string {
	ids := make([]string, len(entries))
	chainOrdinal := map[string]int{}
	plainOrdinal := 0

	for i, e := range entries {
		if c, ok := stringField(e, "comment"); ok && c != "" {
			ids[i] = c
			continue
		}

		switch section {
		case "ip/firewall/filter", "ip/firewall/nat", "ip/firewall/mangle", "ip/firewall/raw":
			chain, _ := stringField(e, "chain")
			if chain == "" {
				chain = "_"
			}
			chainOrdinal[chain]++
			ids[i] = fmt.Sprintf("%s#%d", chain, chainOrdinal[chain])

		case "ip/firewall/address-list":
			list, _ := stringField(e, "list")
			addr, _ := stringField(e, "address")
			if list != "" || addr != "" {
				ids[i] = list + "|" + addr
				continue
			}
			plainOrdinal++
			ids[i] = fmt.Sprintf("#%d", plainOrdinal)

		case "ip/dns/static":
			if addr, ok := stringField(e, "address"); ok && addr != "" {
				ids[i] = addr
				continue
			}
			plainOrdinal++
			ids[i] = fmt.Sprintf("#%d", plainOrdinal)

		case "ip/route":
			dst, _ := stringField(e, "dst-address")
			gw, _ := stringField(e, "gateway")
			ids[i] = dst + "->" + gw

		default:
			if name, ok := stringField(e, "name"); ok && name != "" {
				ids[i] = name
				continue
			}
			if addr, ok := stringField(e, "address"); ok && addr != "" {
				ids[i] = addr
				continue
			}
			plainOrdinal++
			ids[i] = fmt.Sprintf("#%d", plainOrdinal)
		}
	}

	return ids
}
