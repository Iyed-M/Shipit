package shipfile

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMissingFileGivesDefaults(t *testing.T) {
	f, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if f.Resources.Memory != DefaultMemory || f.Resources.CPUs != DefaultCPUs || f.Test.Command != "" {
		t.Errorf("file = %+v", f)
	}
}

func TestParsesAndKeepsDefaultsForUnsetKeys(t *testing.T) {
	f, err := Load(write(t, `
[test]
command = "npm test"

[health]
path = "/healthz"

[resources]
cpus = 1.5
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Test.Command != "npm test" || f.Health.Path != "/healthz" {
		t.Errorf("file = %+v", f)
	}
	if f.Resources.CPUs != 1.5 || f.Resources.Memory != DefaultMemory {
		t.Errorf("resources = %+v", f.Resources)
	}
}

func TestRejectsInvalidFiles(t *testing.T) {
	for name, content := range map[string]string{
		"syntax":      "[test",
		"unknown key": "[tests]\ncommand = \"x\"\n",
	} {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
