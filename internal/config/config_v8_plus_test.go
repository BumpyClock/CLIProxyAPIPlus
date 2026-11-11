package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestV8MigrationPreservesPlusNativeSettings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "plus-v8-native.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var legacy yaml.Node
	if err = yaml.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if IsV8ConfigLayout(legacy.Content[0]) {
		t.Fatal("native Plus settings must not turn a legacy document into v8")
	}
	migrated, _, err := NormalizeConfigLayout(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(migrated); err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err = yaml.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	delete(expected, "port")
	assertSettings := func(data []byte) {
		t.Helper()
		var actual map[string]any
		if errDecode := yaml.Unmarshal(data, &actual); errDecode != nil {
			t.Fatal(errDecode)
		}
		for key, value := range expected {
			if !reflect.DeepEqual(actual[key], value) {
				t.Errorf("native Plus setting %s changed: got %#v, want %#v", key, actual[key], value)
			}
		}
	}
	assertSettings(migrated)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, migrated, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Debug = true
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertSettings(saved)
	if err = ValidateV8Config(saved); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Debug || len(loaded.FallbackChain) != 1 || len(loaded.KiroKey) != 1 || loaded.AmpCode.UpstreamURL != "https://amp.example.invalid" || !loaded.IncognitoBrowser {
		t.Fatal("unrelated legacy save lost effective Plus settings")
	}
}

func TestV8ValidationRejectsUnknownPlusSettings(t *testing.T) {
	for _, raw := range []string{
		"config-version: 8\nampcode: {unknown-setting: true}\n",
		"config-version: 8\nkiro-fingerprint: {unknown-setting: true}\n",
		"config-version: 8\nkiro-rate-limit: {unknown-setting: true}\n",
		"config-version: 8\nkiro: [{unknown-setting: true}]\n",
	} {
		if err := ValidateV8Config([]byte(raw)); err == nil {
			t.Errorf("unknown native field accepted: %s", raw)
		}
	}
}
