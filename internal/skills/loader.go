package skills

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Skill struct {
	Name         string
	Description  string
	Trigger      string
	ContentPath  string
	Content      string
	AllowedTools []string
}

type Catalog struct {
	Skills []Skill
}

type SkillConfig struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Trigger      string   `yaml:"trigger"`
	ContentPath  string   `yaml:"content_path"`
	AllowedTools []string `yaml:"allowed_tools"`
}

type dirEntry struct {
	dirName string
	cfg     SkillConfig
}

func Scan(dir string) *Catalog {
	if _, err := os.Stat(dir); err != nil {
		return &Catalog{}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return &Catalog{}
	}

	var candidates []dirEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		yamlPath := filepath.Join(dir, e.Name(), "skill.yaml")
		data, err := os.ReadFile(yamlPath)
		if err != nil {
			continue
		}
		var cfg SkillConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			slog.Warn("skills: invalid yaml", "path", yamlPath, "err", err)
			continue
		}
		if strings.TrimSpace(cfg.Name) == "" {
			slog.Warn("skills: empty name", "path", yamlPath)
			continue
		}
		candidates = append(candidates, dirEntry{dirName: e.Name(), cfg: cfg})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].dirName < candidates[j].dirName
	})

	seen := make(map[string]bool)
	var skills []Skill
	for _, c := range candidates {
		if seen[c.cfg.Name] {
			slog.Warn("skills: duplicate name, skipped", "skill", c.cfg.Name)
			continue
		}
		seen[c.cfg.Name] = true

		skillDir := filepath.Join(dir, c.dirName)
		contentFile := filepath.Join(skillDir, c.cfg.ContentPath)
		rel, err := filepath.Rel(skillDir, contentFile)
		if err != nil || strings.HasPrefix(rel, "..") {
			slog.Warn("skills: content_path escapes skill directory", "skill", c.cfg.Name, "path", c.cfg.ContentPath)
			continue
		}
		data, err := os.ReadFile(contentFile)
		if err != nil {
			slog.Warn("skills: content_path not found", "skill", c.cfg.Name, "path", contentFile)
			delete(seen, c.cfg.Name)
			continue
		}

		allowed := c.cfg.AllowedTools
		if allowed == nil {
			allowed = []string{}
		}

		skills = append(skills, Skill{
			Name:         c.cfg.Name,
			Description:  c.cfg.Description,
			Trigger:      c.cfg.Trigger,
			ContentPath:  c.cfg.ContentPath,
			Content:      string(data),
			AllowedTools: allowed,
		})
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return &Catalog{Skills: skills}
}

func (c *Catalog) Get(name string) *Skill {
	for i := range c.Skills {
		if c.Skills[i].Name == name {
			return &c.Skills[i]
		}
	}
	return nil
}

func (c *Catalog) Names() []string {
	names := make([]string, len(c.Skills))
	for i, s := range c.Skills {
		names[i] = s.Name
	}
	return names
}
