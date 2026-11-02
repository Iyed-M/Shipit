// Package shipfile reads the optional shipit.toml at the root of a repo.
package shipfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Name is the file looked for in a repo.
const Name = "shipit.toml"

// Default limits applied to every app unless shipit.toml overrides them.
const (
	DefaultMemory = "512m"
	DefaultCPUs   = 0.5
)

// memoryLimit is the form docker run accepts, such as 512m or 1g.
var memoryLimit = regexp.MustCompile(`(?i)^[1-9][0-9]*[bkmg]?$`)

// File is the parsed contents of shipit.toml.
type File struct {
	Test struct {
		Command string `toml:"command"`
	} `toml:"test"`
	Health struct {
		Path string `toml:"path"`
	} `toml:"health"`
	Resources struct {
		Memory string  `toml:"memory"`
		CPUs   float64 `toml:"cpus"`
	} `toml:"resources"`
}

// Load reads shipit.toml from dir. A missing file gives the defaults.
func Load(dir string) (File, error) {
	f := File{}
	f.Resources.Memory = DefaultMemory
	f.Resources.CPUs = DefaultCPUs

	data, err := os.ReadFile(filepath.Join(dir, Name))
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", Name, err)
	}

	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return File{}, fmt.Errorf("parse %s: %w", Name, err)
	}
	if unknown := md.Undecoded(); len(unknown) > 0 {
		return File{}, fmt.Errorf("parse %s: unknown key %q", Name, unknown[0].String())
	}
	if err := f.validate(); err != nil {
		return File{}, fmt.Errorf("invalid %s: %w", Name, err)
	}
	return f, nil
}

func (f File) validate() error {
	if f.Health.Path != "" && !strings.HasPrefix(f.Health.Path, "/") {
		return fmt.Errorf("health.path %q must start with /", f.Health.Path)
	}
	// Docker reads 0 as no limit, so these checks are what keep an app capped.
	if !memoryLimit.MatchString(f.Resources.Memory) {
		return fmt.Errorf("resources.memory %q must look like 512m or 1g", f.Resources.Memory)
	}
	if f.Resources.CPUs <= 0 {
		return fmt.Errorf("resources.cpus must be above 0, got %v", f.Resources.CPUs)
	}
	return nil
}
