package node

import (
	"net/url"
	"sort"
	"strings"

	"hivecrawl/internal/types"
)

// rrfK is the standard reciprocal-rank-fusion constant.
const rrfK = 60

// normalizeURL is the dedupe key: lowercase host, no www, no fragment,
// no trailing slash.
func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	key := strings.TrimPrefix(strings.ToLower(u.Host), "www.") + strings.TrimSuffix(u.Path, "/")
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

// merge combines ranked lists (e.g. local + a peer's). A URL found in
// several lists ranks higher; engines are unioned and the longest
// description wins. Lists earlier in the slice win ties.
func merge(lists [][]types.Result) []types.Result {
	type acc struct {
		r     types.Result
		score float64
		order int
	}
	byKey := map[string]*acc{}
	n := 0
	for _, list := range lists {
		for i, r := range list {
			k := normalizeURL(r.URL)
			a, ok := byKey[k]
			if !ok {
				a = &acc{r: r, order: n}
				a.r.Engines = append([]string(nil), r.Engines...)
				byKey[k] = a
				n++
			} else {
				for _, e := range r.Engines {
					if !contains(a.r.Engines, e) {
						a.r.Engines = append(a.r.Engines, e)
					}
				}
				if len(r.Description) > len(a.r.Description) {
					a.r.Description = r.Description
				}
			}
			a.score += 1.0 / float64(rrfK+i+1)
		}
	}
	all := make([]*acc, 0, len(byKey))
	for _, a := range byKey {
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].order < all[j].order
	})
	out := make([]types.Result, len(all))
	for i, a := range all {
		out[i] = a.r
		out[i].Position = i + 1
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
