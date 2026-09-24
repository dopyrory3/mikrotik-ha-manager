package model

import "fmt"

// Entry is one row of a RouterOS REST section response, keyed by RouterOS
// field name (e.g. "chain", "comment", "dynamic").
type Entry map[string]any

func stringField(e Entry, key string) (string, bool) {
	v, ok := e[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func toComparable(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
