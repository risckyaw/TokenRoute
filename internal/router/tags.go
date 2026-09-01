package router

import "strings"

// TagSelector is a parsed X-Route-Tags header (LiteLLM tag_based_routing):
// plain tags require candidate membership (subset), "!tag" excludes
// candidates carrying it, "&tag" requires it.
type TagSelector struct {
	Plain   []string
	Exclude []string
	Require []string
}

// ParseTagSelector parses a comma-separated tag header; empty -> nil
// (all candidates pass). Whitespace trimmed, empty entries dropped.
func ParseTagSelector(header string) *TagSelector {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	sel := &TagSelector{}
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		switch t[0] {
		case '!':
			if len(t) > 1 {
				sel.Exclude = append(sel.Exclude, t[1:])
			}
		case '&':
			if len(t) > 1 {
				sel.Require = append(sel.Require, t[1:])
			}
		default:
			sel.Plain = append(sel.Plain, t)
		}
	}
	if len(sel.Plain) == 0 && len(sel.Exclude) == 0 && len(sel.Require) == 0 {
		return nil
	}
	return sel
}

// MatchTags reports whether a candidate's tags satisfy the selector: all
// &required present, no !excluded present, and all plain tags present
// (subset). Empty candidate tags match everything except requirements.
func (sel *TagSelector) MatchTags(candidateTags []string) bool {
	if sel == nil {
		return true
	}
	have := make(map[string]bool, len(candidateTags))
	for _, t := range candidateTags {
		have[t] = true
	}
	for _, t := range sel.Require {
		if !have[t] {
			return false
		}
	}
	for _, t := range sel.Exclude {
		if have[t] {
			return false
		}
	}
	for _, t := range sel.Plain {
		if !have[t] {
			return false
		}
	}
	return true
}
