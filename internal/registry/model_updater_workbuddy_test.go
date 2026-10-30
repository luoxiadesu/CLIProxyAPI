package registry

import "testing"

func seedEmbeddedCatalogForTest(t *testing.T) {
	t.Helper()
	modelsCatalogStore.mu.RLock()
	previous := modelsCatalogStore.data
	modelsCatalogStore.mu.RUnlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = previous
		modelsCatalogStore.mu.Unlock()
	})
	if errLoad := loadModelsFromBytes(embeddedModelsJSON, "embed"); errLoad != nil {
		t.Fatalf("load embedded catalog: %v", errLoad)
	}
	if len(GetWorkBuddyModels()) == 0 {
		t.Fatal("embedded catalog has no workbuddy models")
	}
}

func TestPublishCatalogBytesPreservesLocalWorkBuddySection(t *testing.T) {
	seedEmbeddedCatalogForTest(t)
	before := GetWorkBuddyModels()

	// The remote catalog has no local-only workbuddy section.
	changed, errPublish := publishCatalogBytes([]byte(`{}`))
	if errPublish != nil {
		t.Fatalf("publish remote catalog: %v", errPublish)
	}
	if containsProviderName(changed, "workbuddy") {
		t.Fatalf("workbuddy reported as changed: %v", changed)
	}
	after := GetWorkBuddyModels()
	if len(after) != len(before) {
		t.Fatalf("workbuddy models after remote refresh = %d, want %d", len(after), len(before))
	}
	for index := range before {
		if after[index].ID != before[index].ID {
			t.Fatalf("workbuddy models[%d] = %q, want %q", index, after[index].ID, before[index].ID)
		}
	}
}

func TestPublishCatalogBytesReplacesWorkBuddyWhenRemoteProvidesIt(t *testing.T) {
	seedEmbeddedCatalogForTest(t)

	remote := []byte(`{"workbuddy":[{"id":"remote-only","object":"model","type":"workbuddy","context_length":8192}]}`)
	changed, errPublish := publishCatalogBytes(remote)
	if errPublish != nil {
		t.Fatalf("publish remote catalog: %v", errPublish)
	}
	if !containsProviderName(changed, "workbuddy") {
		t.Fatalf("workbuddy change not reported: %v", changed)
	}
	models := GetWorkBuddyModels()
	if len(models) != 1 || models[0].ID != "remote-only" {
		t.Fatalf("workbuddy models = %+v, want remote-only", models)
	}
}

func TestGetWorkBuddyModelsReturnsClones(t *testing.T) {
	models := GetWorkBuddyModels()
	if len(models) == 0 {
		t.Skip("embedded catalog has no workbuddy models")
	}
	models[0].ID = "mutated"
	if again := GetWorkBuddyModels(); again[0].ID == "mutated" {
		t.Fatal("GetWorkBuddyModels returned a shared pointer")
	}
}

func containsProviderName(providers []string, want string) bool {
	for _, provider := range providers {
		if provider == want {
			return true
		}
	}
	return false
}
