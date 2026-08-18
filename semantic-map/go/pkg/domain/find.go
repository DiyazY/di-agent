package domain

import (
	"fmt"
	"os"
	"path/filepath"
)

// FindSpec locates the committed domain_spec.json by walking up from dir. It
// exists so callers — tests, tools, and the daemon's convenience fallback — do
// not each hardcode a repo layout. Pass "" to start from the working directory.
func FindSpec(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "domain_spec.json")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("domain_spec.json not found above %q", dir)
}

// LoadFound finds and loads the committed spec in one step.
func LoadFound() (*Spec, error) {
	p, err := FindSpec("")
	if err != nil {
		return nil, err
	}
	return Load(p)
}
