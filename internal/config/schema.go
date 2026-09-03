package config

import (
	"reflect"
	"strings"
)

type FieldSchema struct {
	Path            string         `json:"path"`
	Name            string         `json:"name"`
	Label           string         `json:"label"`
	Help            string         `json:"help,omitempty"`
	Kind            string         `json:"kind"`
	Section         string         `json:"section,omitempty"`
	Advanced        bool           `json:"advanced,omitempty"`
	Secret          bool           `json:"secret,omitempty"`
	RestartRequired bool           `json:"restart_required,omitempty"`
	Enum            []string       `json:"enum,omitempty"`
	Identity        []string       `json:"identity,omitempty"`
	VisibleWhen     *Visibility    `json:"visible_when,omitempty"`
	Children        []*FieldSchema `json:"children,omitempty"`
	Item            *FieldSchema   `json:"item,omitempty"`
}

type Visibility struct {
	Path   string   `json:"path"`
	Values []string `json:"values"`
}

type fieldMetadata struct {
	Section         string
	Help            string
	Advanced        bool
	Secret          bool
	RestartRequired bool
	Enum            []string
	Identity        []string
	VisibleWhen     *Visibility
}

// fieldMeta holds explicit metadata keyed by wildcard schema paths
// ([] for list items, .* for map entries).
var fieldMeta = map[string]fieldMetadata{
	"listen":                 {Section: "general", RestartRequired: true},
	"admin_listen":           {Section: "general", RestartRequired: true},
	"usage_db":               {Section: "general", RestartRequired: true},
	"admin_key":              {Section: "general", Secret: true, RestartRequired: true},
	"max_body_mb":            {Section: "general"},
	"cache":                  {Section: "general"},
	"model_catalog":          {Section: "general"},
	"pricing_sync":           {Section: "pricing"},
	"providers":              {Section: "providers", Identity: []string{"name"}},
	"providers[].type":       {Enum: []string{"openai", "anthropic", "gemini"}},
	"providers[].api_key":    {Secret: true},
	"providers[].api_keys[]": {Secret: true},
	"routes":                 {Section: "routes", Identity: []string{"model"}},
	"routes[].strategy":      {Enum: strategyNames()},
	"routes[].candidates":    {Identity: []string{"provider", "model"}},
	"prices":                 {Section: "pricing"},
	"free_tier":              {Section: "pricing", Identity: []string{"provider", "model"}},
	"aliases":                {Section: "pricing"},
	"group_ratio":            {Section: "pricing"},
	"retry_policy":           {Section: "resilience"},
	"failure_rules":          {Section: "resilience", Identity: []string{"match", "status"}},
	"search":                 {Section: "search", Identity: []string{"backend"}},
	"search[].backend":       {Enum: []string{"tavily", "brave", "exa"}},
	"search[].api_key":       {Secret: true},
	"search[].api_keys[]":    {Secret: true},
	"prompt_cache_affinity":  {Section: "resilience"},
	"health_check":           {Section: "resilience"},
	// Advanced metadata: less-common nested/provider/route fields.
	"providers[].priority":                   {Advanced: true},
	"providers[].response_header_timeout_ms": {Advanced: true},
	"providers[].stream_idle_timeout_ms":     {Advanced: true},
	"providers[].model_mapping":              {Advanced: true},
	"providers[].circuit":                    {Advanced: true},
	"providers[].quota_token_limit":          {Advanced: true},
	"providers[].quota_window_seconds":       {Advanced: true},
	"providers[].health_check":               {Advanced: true},
	"providers[].param_override":             {Advanced: true},
	"providers[].param_delete":               {Advanced: true},
	"providers[].header_override":            {Advanced: true},
	"providers[].header_pass":                {Advanced: true},
	"providers[].balance_probe":              {Advanced: true},
	"routes[].multiplier":                    {Advanced: true},
	"routes[].fallback_routes":               {Advanced: true},
	"routes[].prompt_cache_affinity":         {Advanced: true},
	"routes[].affinity":                      {Advanced: true},
	"routes[].candidates[].weight":           {Advanced: true},
	"routes[].candidates[].groups":           {Advanced: true},
	"routes[].candidates[].tags":             {Advanced: true},
	"routes[].candidates[].param_override":   {Advanced: true},
	"prices.*.embed_per_1m":                  {Advanced: true},
	"prices.*.context_tokens":                {Advanced: true},
	"prices.*.expr":                          {Advanced: true},
	"retry_policy.never_retry":               {Advanced: true},
	"retry_policy.disable_status_ranges":     {Advanced: true},
	"retry_policy.disable_keywords":          {Advanced: true},
	"failure_rules[].backoff":                {Advanced: true},
	// VisibleWhen: strategy-conditional route fields.
	"routes[].hash_on": {
		Advanced:    true,
		VisibleWhen: &Visibility{Path: "routes[].strategy", Values: []string{"consistent_hash"}},
	},
	"routes[].sticky": {
		Advanced:    true,
		VisibleWhen: &Visibility{Path: "routes[].strategy", Values: []string{"round_robin"}},
	},
	"routes[].fusion_judge": {
		Advanced:    true,
		VisibleWhen: &Visibility{Path: "routes[].strategy", Values: []string{"fusion_judge"}},
	},
}

// strategyNames is the single source of router strategy names for both
// schema metadata and config validation (same package, no import cost).
func strategyNames() []string {
	return []string{
		"priority", "round_robin", "least_latency", "weighted", "cost", "lkgp",
		"headroom", "fusion", "p2c", "reset_aware", "fill_first", "auto",
		"lowest_usage", "peak_ewma", "least_connections", "consistent_hash",
		"fusion_judge",
	}
}

func labelFor(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func kindOf(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int64, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "list"
	case reflect.Map:
		return "map"
	case reflect.Struct:
		return "group"
	}
	return "string"
}

// build recursively derives the schema from Go types, applying fieldMeta.
func build(t reflect.Type, path, name, section string) *FieldSchema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	f := &FieldSchema{
		Path:    path,
		Name:    name,
		Label:   labelFor(name),
		Kind:    kindOf(t),
		Section: section,
	}
	if m, ok := fieldMeta[path]; ok {
		if m.Section != "" {
			f.Section = m.Section
		}
		f.Help = m.Help
		f.Advanced = m.Advanced
		f.Secret = m.Secret
		f.RestartRequired = m.RestartRequired
		f.Enum = m.Enum
		f.Identity = m.Identity
		f.VisibleWhen = m.VisibleWhen
	}
	// unclassified root field -> advanced
	if section == "" && f.Section == "" && !strings.Contains(path, ".") && path != "" {
		f.Advanced = true
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		et := t.Elem()
		for et.Kind() == reflect.Pointer {
			et = et.Elem()
		}
		if et.Kind() == reflect.Struct {
			f.Item = build(et, path+"[]", name, f.Section)
		} else {
			f.Item = &FieldSchema{Path: path + "[]", Name: name, Label: labelFor(name), Kind: kindOf(et), Section: f.Section}
			if m, ok := fieldMeta[path+"[]"]; ok {
				applyItemMeta(f.Item, m)
			}
		}
	case reflect.Map:
		vt := t.Elem()
		for vt.Kind() == reflect.Pointer {
			vt = vt.Elem()
		}
		if vt.Kind() == reflect.Struct {
			// expose map value fields under ".*" children so coverage sees them
			for i := 0; i < vt.NumField(); i++ {
				sf := vt.Field(i)
				tag := strings.Split(sf.Tag.Get("yaml"), ",")[0]
				if tag == "" || tag == "-" {
					continue
				}
				f.Children = append(f.Children, build(sf.Type, path+".*."+tag, tag, f.Section))
			}
		}
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			tag := strings.Split(sf.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			childPath := tag
			if path != "" {
				childPath = path + "." + tag
			}
			f.Children = append(f.Children, build(sf.Type, childPath, tag, f.Section))
		}
	}
	return f
}

func applyItemMeta(f *FieldSchema, m fieldMetadata) {
	if m.Section != "" {
		f.Section = m.Section
	}
	f.Help = m.Help
	f.Advanced = m.Advanced
	f.Secret = m.Secret
	f.RestartRequired = m.RestartRequired
	f.Enum = m.Enum
	f.Identity = m.Identity
	f.VisibleWhen = m.VisibleWhen
}

func FormSchema() *FieldSchema {
	root := &FieldSchema{Path: "", Name: "config", Label: "Configuration", Kind: "group"}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		tag := strings.Split(sf.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		root.Children = append(root.Children, build(sf.Type, tag, tag, ""))
	}
	return root
}

func RestartRequiredPaths() []string {
	var out []string
	var walk func(f *FieldSchema)
	walk = func(f *FieldSchema) {
		if f == nil {
			return
		}
		if f.RestartRequired {
			out = append(out, f.Path)
		}
		walk(f.Item)
		for _, c := range f.Children {
			walk(c)
		}
	}
	for _, c := range FormSchema().Children {
		walk(c)
	}
	return out
}

func IsSecretPath(path string) bool {
	if m, ok := fieldMeta[path]; ok && m.Secret {
		return true
	}
	var found bool
	var walk func(f *FieldSchema)
	walk = func(f *FieldSchema) {
		if f == nil || found {
			return
		}
		if f.Path == path && f.Secret {
			found = true
			return
		}
		walk(f.Item)
		for _, c := range f.Children {
			walk(c)
		}
	}
	for _, c := range FormSchema().Children {
		walk(c)
	}
	return found
}
