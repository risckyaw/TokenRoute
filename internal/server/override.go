package server

import (
	"encoding/json"
	"path"
	"strings"
)

// ParamOps applies set-only body overrides (new-api override.go, reduced):
// provider-level sets/deletes first, then candidate-level sets (candidate
// wins on key conflict). Only JSON object bodies are touched; anything else
// returns unchanged.
func ParamOps(body []byte, providerSet map[string]any, providerDel []string, candidateSet map[string]any) []byte {
	if len(providerSet) == 0 && len(providerDel) == 0 && len(candidateSet) == 0 {
		return body
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
		return body // non-object body untouched
	}
	for _, k := range providerDel {
		delete(parsed, k)
	}
	for k, v := range providerSet {
		parsed[k] = v
	}
	for k, v := range candidateSet {
		parsed[k] = v
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return out
}

// HeaderPassMatch reports whether a client header name matches any glob
// (case-insensitive, path.Match per segment — "*" spans one token incl.
// dashes since globs match the whole name, not path separators here).
func HeaderPassMatch(patterns []string, name string) bool {
	name = strings.ToLower(name)
	for _, p := range patterns {
		p = strings.ToLower(p)
		// path.Match treats '-' literally; '*' matches any run of non-'/'.
		// Header names never contain '/', so a plain glob works.
		if ok, _ := path.Match(p, name); ok {
			return true
		}
		if p == name {
			return true
		}
	}
	return false
}
