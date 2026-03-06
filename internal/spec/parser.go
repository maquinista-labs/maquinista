package spec

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SpecFile represents a parsed task specification file.
type SpecFile struct {
	ID               string   `yaml:"id"`
	Title            string   `yaml:"title"`
	Priority         int      `yaml:"priority"`
	DependsOn        []string `yaml:"depends_on"`
	TestCmd          string   `yaml:"test_cmd"`
	RequiresApproval bool     `yaml:"requires_approval"`
	Body             string   `yaml:"-"`
	FilePath         string   `yaml:"-"`
}

var frontmatterSep = []byte("---")

// ParseFile parses a spec file with YAML frontmatter and markdown body.
func ParseFile(path string) (*SpecFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading spec file: %w", err)
	}

	spec, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	spec.FilePath = path
	return spec, nil
}

func parse(data []byte) (*SpecFile, error) {
	data = bytes.TrimSpace(data)

	if !bytes.HasPrefix(data, frontmatterSep) {
		return nil, fmt.Errorf("missing frontmatter separator")
	}

	// Skip opening ---
	rest := data[len(frontmatterSep):]
	idx := bytes.Index(rest, frontmatterSep)
	if idx < 0 {
		return nil, fmt.Errorf("missing closing frontmatter separator")
	}

	frontmatter := rest[:idx]
	body := bytes.TrimSpace(rest[idx+len(frontmatterSep):])

	var spec SpecFile
	if err := yaml.Unmarshal(frontmatter, &spec); err != nil {
		return nil, fmt.Errorf("parsing frontmatter YAML: %w", err)
	}

	spec.Body = string(body)
	return &spec, nil
}

// ParseDir parses all .md files in a directory.
func ParseDir(dir string) ([]*SpecFile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("globbing spec dir: %w", err)
	}

	var specs []*SpecFile
	for _, path := range matches {
		spec, err := ParseFile(path)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}
