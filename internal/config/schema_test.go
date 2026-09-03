package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// collectYAMLPaths walks every yaml tag reachable from t, returning wildcard
// schema paths: slices become name[], nested structs name.child, maps name
// (their value types are walked as name.* only for struct values).
func collectYAMLPaths(t reflect.Type, prefix string) map[string]bool {
	out := map[string]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Slice, reflect.Array:
			walk(t.Elem(), path+"[]")
			return
		case reflect.Map:
			vt := t.Elem()
			for vt.Kind() == reflect.Pointer {
				vt = vt.Elem()
			}
			if vt.Kind() == reflect.Struct {
				walk(vt, path+".*")
			}
			return
		case reflect.Struct:
		default:
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("yaml")
			if tag == "" || tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				continue
			}
			p := name
			if path != "" {
				p = path + "." + name
			}
			out[p] = true
			walk(f.Type, p)
		}
	}
	walk(t, prefix)
	return out
}

// flattenSchemaPaths returns every path in the schema tree; list nodes also
// register their []-suffixed form so yaml slice tags match.
func flattenSchemaPaths(root *FieldSchema) map[string]bool {
	out := map[string]bool{}
	var walk func(f *FieldSchema)
	walk = func(f *FieldSchema) {
		if f == nil {
			return
		}
		out[f.Path] = true
		if f.Item != nil {
			out[f.Path+"[]"] = true
			walk(f.Item)
		}
		for _, c := range f.Children {
			walk(c)
		}
	}
	for _, c := range root.Children {
		walk(c)
	}
	return out
}

func findField(t *testing.T, root *FieldSchema, path string) *FieldSchema {
	t.Helper()
	var found *FieldSchema
	var walk func(f *FieldSchema)
	walk = func(f *FieldSchema) {
		if f == nil || found != nil {
			return
		}
		if f.Path == path {
			found = f
			return
		}
		walk(f.Item)
		for _, c := range f.Children {
			walk(c)
		}
	}
	for _, c := range root.Children {
		walk(c)
	}
	// fallback: match the list node by its []-suffixed form
	if found == nil {
		for _, c := range root.Children {
			walkList(c, path, &found)
		}
	}
	if found == nil {
		t.Fatalf("schema has no field %s", path)
	}
	return found
}

func walkList(f *FieldSchema, path string, found **FieldSchema) {
	if f == nil || *found != nil {
		return
	}
	if f.Path+"[]" == path {
		*found = f
		return
	}
	walkList(f.Item, path, found)
	for _, c := range f.Children {
		walkList(c, path, found)
	}
}

func assertSecret(t *testing.T, path string) {
	t.Helper()
	if f := findField(t, FormSchema(), path); !f.Secret {
		t.Errorf("%s: want Secret", path)
	}
}

func assertRestart(t *testing.T, path string) {
	t.Helper()
	if f := findField(t, FormSchema(), path); !f.RestartRequired {
		t.Errorf("%s: want RestartRequired", path)
	}
}

func assertEnum(t *testing.T, path string, values ...string) {
	t.Helper()
	f := findField(t, FormSchema(), path)
	have := map[string]bool{}
	for _, v := range f.Enum {
		have[v] = true
	}
	for _, v := range values {
		if !have[v] {
			t.Errorf("%s: enum missing %q", path, v)
		}
	}
}

func assertIdentity(t *testing.T, path string, keys ...string) {
	t.Helper()
	f := findField(t, FormSchema(), path)
	if !reflect.DeepEqual(f.Identity, keys) {
		t.Errorf("%s: identity = %v, want %v", path, f.Identity, keys)
	}
}

func TestFormSchemaCoversEveryYAMLField(t *testing.T) {
	want := collectYAMLPaths(reflect.TypeOf(Config{}), "")
	got := flattenSchemaPaths(FormSchema())
	for path := range want {
		if !got[path] {
			t.Errorf("schema missing %s", path)
		}
	}
}

func TestFormSchemaCriticalMetadata(t *testing.T) {
	assertSecret(t, "providers[].api_key")
	assertSecret(t, "search[].api_keys[]")
	assertRestart(t, "listen")
	assertRestart(t, "admin_key")
	assertEnum(t, "routes[].strategy", "priority", "fusion_judge", "consistent_hash")
	assertIdentity(t, "providers", "name")
	assertIdentity(t, "routes", "model")
}

func TestSecretAndRestartHelpers(t *testing.T) {
	if !IsSecretPath("providers[].api_key") {
		t.Error("IsSecretPath(providers[].api_key) = false")
	}
	if IsSecretPath("listen") {
		t.Error("IsSecretPath(listen) = true")
	}
	paths := RestartRequiredPaths()
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	for _, p := range []string{"listen", "admin_listen", "usage_db", "admin_key"} {
		if !have[p] {
			t.Errorf("RestartRequiredPaths missing %s", p)
		}
	}
}

func TestFormSchemaAdvancedMetadata(t *testing.T) {
	// representative common fields stay common
	for _, path := range []string{
		"listen",
		"providers[].name",
		"providers[].type",
		"providers[].base_url",
		"routes[].model",
		"routes[].strategy",
		"routes[].candidates[].provider",
		"routes[].candidates[].model",
		"search[].backend",
		"prices.*.prompt_per_1m",
		"retry_policy.retry_status_ranges",
		"failure_rules[].match",
	} {
		if f := findField(t, FormSchema(), path); f.Advanced {
			t.Errorf("%s: want Advanced=false (common field)", path)
		}
	}
	// less-common nested/provider/route fields are Advanced
	for _, path := range []string{
		"providers[].response_header_timeout_ms",
		"providers[].stream_idle_timeout_ms",
		"providers[].model_mapping",
		"providers[].circuit",
		"providers[].quota_token_limit",
		"providers[].health_check",
		"providers[].param_override",
		"providers[].header_pass",
		"providers[].balance_probe",
		"routes[].multiplier",
		"routes[].fallback_routes",
		"routes[].prompt_cache_affinity",
		"routes[].affinity",
		"routes[].hash_on",
		"routes[].sticky",
		"routes[].fusion_judge",
		"routes[].candidates[].weight",
		"routes[].candidates[].tags",
		"prices.*.expr",
		"retry_policy.disable_keywords",
		"failure_rules[].backoff",
	} {
		if f := findField(t, FormSchema(), path); !f.Advanced {
			t.Errorf("%s: want Advanced=true", path)
		}
	}
}

func TestFormSchemaVisibleWhen(t *testing.T) {
	cases := []struct {
		path       string
		wantPath   string
		wantValues []string
	}{
		{"routes[].hash_on", "routes[].strategy", []string{"consistent_hash"}},
		{"routes[].sticky", "routes[].strategy", []string{"round_robin"}},
		{"routes[].fusion_judge", "routes[].strategy", []string{"fusion_judge"}},
	}
	for _, tc := range cases {
		f := findField(t, FormSchema(), tc.path)
		if f.VisibleWhen == nil {
			t.Errorf("%s: want VisibleWhen, got nil", tc.path)
			continue
		}
		if f.VisibleWhen.Path != tc.wantPath {
			t.Errorf("%s: VisibleWhen.Path = %q, want %q", tc.path, f.VisibleWhen.Path, tc.wantPath)
		}
		if !reflect.DeepEqual(f.VisibleWhen.Values, tc.wantValues) {
			t.Errorf("%s: VisibleWhen.Values = %v, want %v", tc.path, f.VisibleWhen.Values, tc.wantValues)
		}
	}
	// a common field must NOT carry VisibleWhen
	if f := findField(t, FormSchema(), "routes[].strategy"); f.VisibleWhen != nil {
		t.Errorf("routes[].strategy: unexpected VisibleWhen %+v", f.VisibleWhen)
	}
}

func TestFormSchemaHasNoRuntimeSecrets(t *testing.T) {
	data, err := json.Marshal(FormSchema())
	if err != nil {
		t.Fatal(err)
	}
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	// The schema must be pure metadata: no "value" keys anywhere.
	var check func(v any)
	check = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				if k == "value" || k == "default" {
					t.Errorf("schema leaks key %q", k)
				}
				check(vv)
			}
		case []any:
			for _, vv := range x {
				check(vv)
			}
		}
	}
	check(probe)
}
