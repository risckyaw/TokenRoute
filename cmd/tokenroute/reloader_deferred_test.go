package main

import (
	"context"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/config"
)

func TestRuntimeReloaderDeferredRestartFieldsRemainDeferred(t *testing.T) {
	f := newReloaderFixture(t, nil)
	first := &config.Config{Listen: ":9", AdminListen: ":10", UsageDB: "new.db", AdminKey: "persisted-new-key"}
	if err := f.rel.Apply(context.Background(), first, []string{"admin_key"}); err != nil {
		t.Fatal(err)
	}
	if got := f.rel.active.Load().AdminKey; got != "k" {
		t.Fatalf("first active admin key = %q", got)
	}
	hotOnly := &config.Config{Listen: ":9", AdminListen: ":10", UsageDB: "new.db", AdminKey: "persisted-new-key", ModelCatalog: "off"}
	if err := f.rel.Apply(context.Background(), hotOnly, []string{}); err != nil {
		t.Fatal(err)
	}
	if got := f.rel.active.Load().AdminKey; got != "k" {
		t.Fatalf("later hot-only apply activated deferred key %q", got)
	}
}
