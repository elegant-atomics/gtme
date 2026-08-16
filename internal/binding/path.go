package binding

import (
	"strconv"
	"strings"
)

// atPath walks a dotted path through decoded JSON: map keys and numeric array
// indexes ("organization.primary_domain", "experience.0.position"). It is the
// whole extraction language — deliberately not JSONPath, because filters and
// predicates are logic (SPEC §10a graduation rule).
func atPath(doc any, path string) any {
	cur := doc
	if strings.TrimSpace(path) == "" || path == "." {
		return cur
	}
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil
			}
			cur = node[i]
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

// atPaths tries alternatives ('|'-separated within one string, or several
// strings), first non-empty wins.
func atPaths(doc any, paths []string) any {
	for _, p := range paths {
		for _, alt := range strings.Split(p, "|") {
			if v := atPath(doc, strings.TrimSpace(alt)); !isEmpty(v) {
				return v
			}
		}
	}
	return nil
}
