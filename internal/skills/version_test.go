package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSkillVersionFromDir(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "version without v prefix",
			content: "---\nname: foo\nversion: 1.0.0\n---\n\n# Foo Skill\n",
			want:    "1.0.0",
		},
		{
			name:    "version with v prefix - strips v",
			content: "---\nname: foo\nversion: v1.0.0\n---\n\n# Foo Skill\n",
			want:    "1.0.0",
		},
		{
			name:    "version with v prefix and complex semver",
			content: "---\nname: foo\nversion: v2.1.3-beta.1\n---\n\n# Foo Skill\n",
			want:    "2.1.3-beta.1",
		},
		{
			name:    "no version field",
			content: "---\nname: foo\ndescription: bar\n---\n\n# Foo Skill\n",
			want:    "",
		},
		{
			name:    "no frontmatter",
			content: "# Just a readme\n\nNo frontmatter here.\n",
			want:    "",
		},
		{
			name:    "empty file",
			content: "",
			want:    "",
		},
		{
			name:    "malformed frontmatter - no closing",
			content: "---\nname: foo\nversion: 1.0.0\n",
			want:    "",
		},
		{
			name: "multiline frontmatter with version",
			content: `---
name: my-skill
description: A test skill
version: 3.0.0
type: tool
---
# My Skill
`,
			want: "3.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.content != "" {
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(tt.content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			got := readSkillVersionFromDir(dir)
			if got != tt.want {
				t.Errorf("readSkillVersionFromDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadSkillVersionFromDir_NoSkillMD(t *testing.T) {
	dir := t.TempDir()
	if got := readSkillVersionFromDir(dir); got != "" {
		t.Errorf("expected empty string when SKILL.md missing, got %q", got)
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"owner/repo", "owner", "repo", true},
		{"owner/repo/subpath", "", "", false},
		{"owner", "", "", false},
		{"", "", "", false},
		{"https://github.com/owner/repo", "", "", false},
		{"owner/repo@skill", "", "", false},
		{"/owner/repo", "", "", false},
		{" owner/repo ", "owner", "repo", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			owner, repo, ok := SplitOwnerRepo(tt.input)
			if ok != tt.wantOK {
				t.Errorf("SplitOwnerRepo(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if owner != tt.wantOwner {
				t.Errorf("SplitOwnerRepo(%q) owner = %q, want %q", tt.input, owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("SplitOwnerRepo(%q) repo = %q, want %q", tt.input, repo, tt.wantRepo)
			}
		})
	}
}

func TestPickSkillsShExact(t *testing.T) {
	results := []SkillsShResult{
		{SkillID: "pdf-viewer", Name: "PDF Viewer", Installs: 500},
		{SkillID: "pdf-editor", Name: "PDF Editor", Installs: 1000},
		{SkillID: "code-review", Name: "Code Review", Installs: 200},
	}

	t.Run("exact skillId match wins", func(t *testing.T) {
		got := PickSkillsShExact(results, "pdf-viewer")
		if got == nil || got.SkillID != "pdf-viewer" {
			t.Errorf("PickSkillsShExact should match exact skillId")
		}
	})

	t.Run("falls back to most installed", func(t *testing.T) {
		got := PickSkillsShExact(results, "nonexistent")
		if got == nil || got.SkillID != "pdf-editor" {
			t.Errorf("PickSkillsShExact should return most-installed: got %v", got)
		}
	})

	t.Run("empty results returns nil", func(t *testing.T) {
		if got := PickSkillsShExact(nil, "foo"); got != nil {
			t.Error("PickSkillsShExact on nil should return nil")
		}
		if got := PickSkillsShExact([]SkillsShResult{}, "foo"); got != nil {
			t.Error("PickSkillsShExact on empty should return nil")
		}
	})
}

func TestFilterOut(t *testing.T) {
	got := filterOut([]string{"main", "master"}, "main")
	if len(got) != 1 || got[0] != "master" {
		t.Errorf("filterOut should remove 'main': got %v", got)
	}

	got = filterOut([]string{"main", "master"}, "develop")
	if len(got) != 2 {
		t.Errorf("filterOut with no match should keep all: got %v", got)
	}
}

func TestStripFrontmatterBody(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "strips frontmatter and returns body",
			content: "---\nname: foo\nversion: 1.0.0\n---\n\n# Hello\n\nThis is the body.\n",
			want:    "# Hello\n\nThis is the body.",
		},
		{
			name:    "no frontmatter - returns trimmed content",
			content: "# Just a heading\n\nSome text.\n",
			want:    "# Just a heading\n\nSome text.",
		},
		{
			name:    "frontmatter only - returns raw content unchanged",
			content: "---\nname: foo\n---\n",
			want:    "---\nname: foo\n---",
		},
		{
			name:    "unclosed frontmatter - returns raw content",
			content: "---\nname: foo\nversion: 1.0.0\n",
			want:    "---\nname: foo\nversion: 1.0.0",
		},
		{
			name:    "empty string",
			content: "",
			want:    "",
		},
		{
			name:    "whitespace only",
			content: "   \n  \n",
			want:    "",
		},
		{
			name: "multiline body with markdown",
			content: `---
name: my-skill
description: A test skill
version: 2.0.0
---
## Overview

This skill does things.

- Feature 1
- Feature 2

### Usage

` + "```bash\nnpx skills add foo/bar\n```" + `
`,
			want: "## Overview\n\nThis skill does things.\n\n- Feature 1\n- Feature 2\n\n### Usage\n\n```bash\nnpx skills add foo/bar\n```",
		},
		{
			name:    "frontmatter with dashes in body",
			content: "---\nname: foo\n---\n\n# Title\n\nSome text with --- inside.\n",
			want:    "# Title\n\nSome text with --- inside.",
		},
		{
			name:    "yaml-like content without frontmatter",
			content: "key: value\nanother: thing\n",
			want:    "key: value\nanother: thing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripFrontmatterBody(tt.content)
			if got != tt.want {
				t.Errorf("stripFrontmatterBody() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSortByQueryPrefix(t *testing.T) {
	results := []SkillsShResult{
		{SkillID: "channel-economics", Name: "channel-economics", Installs: 283},
		{SkillID: "silicon-economics", Name: "silicon-economics", Installs: 1},
		{SkillID: "economics", Name: "economics", Installs: 54},
		{SkillID: "crossing-the-chasm", Name: "crossing-the-chasm", Installs: 295},
		{SkillID: "token-economics", Name: "token-economics", Installs: 221},
	}

	t.Run("exact match goes first", func(t *testing.T) {
		cp := make([]SkillsShResult, len(results))
		copy(cp, results)
		SortByQueryPrefix(cp, "silicon-economics")
		if cp[0].SkillID != "silicon-economics" {
			t.Errorf("exact match should be first, got %q", cp[0].SkillID)
		}
	})

	t.Run("prefix match ranks above fuzzy", func(t *testing.T) {
		cp := make([]SkillsShResult, len(results))
		copy(cp, results)
		SortByQueryPrefix(cp, "token")
		// token-economics should be first (prefix match)
		if cp[0].SkillID != "token-economics" {
			t.Errorf("prefix match should be first, got %q", cp[0].SkillID)
		}
	})

	t.Run("empty query is no-op", func(t *testing.T) {
		cp := make([]SkillsShResult, len(results))
		copy(cp, results)
		SortByQueryPrefix(cp, "")
		if cp[0].SkillID != results[0].SkillID {
			t.Errorf("empty query should keep original order")
		}
	})

	t.Run("single result is no-op", func(t *testing.T) {
		single := []SkillsShResult{{SkillID: "foo", Installs: 1}}
		SortByQueryPrefix(single, "foo")
		if len(single) != 1 || single[0].SkillID != "foo" {
			t.Error("single result should be unchanged")
		}
	})

	t.Run("tie-break by installs within same score bucket", func(t *testing.T) {
		cp := []SkillsShResult{
			{SkillID: "silicon-foo", Name: "silicon-foo", Installs: 10},
			{SkillID: "silicon-bar", Name: "silicon-bar", Installs: 50},
		}
		SortByQueryPrefix(cp, "silicon")
		// Both are prefix matches (score=2), so higher installs first
		if cp[0].SkillID != "silicon-bar" {
			t.Errorf("within same score bucket, higher installs first: got %q", cp[0].SkillID)
		}
	})
}
