package skills

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// skillsShBaseURL is the hostname for https://skills.sh. It returns a public
// JSON search endpoint but does not expose per-skill metadata — tarball
// downloads go directly to codeload.github.com using the source repo listed
// in each search result.
const skillsShBaseURL = "https://skills.sh"

// SkillsShResult is one entry returned by the skills.sh search API.
type SkillsShResult struct {
	ID       string `json:"id"`       // "<owner>/<repo>/<skillId>" (display-only)
	SkillID  string `json:"skillId"`  // folder name of the skill inside the source repo
	Name     string `json:"name"`     // human-readable name
	Source   string `json:"source"`   // "<owner>/<repo>" — the GitHub location
	Installs int    `json:"installs"` // popularity hint for ranking
}

// SearchSkillsSh queries https://skills.sh/api/search?q=... and returns the
// raw results. An empty slice means no matches.
func SearchSkillsSh(query string) ([]SkillsShResult, error) {
	u := fmt.Sprintf("%s/api/search?q=%s", skillsShBaseURL, url.QueryEscape(query))
	resp, err := defaultHTTPClient().Get(u)
	if err != nil {
		return nil, fmt.Errorf("skills.sh search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skills.sh search HTTP %d", resp.StatusCode)
	}
	var body struct {
		Skills []SkillsShResult `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode skills.sh: %w", err)
	}
	return body.Skills, nil
}

// PickSkillsShExact returns the result that best matches name: exact skillId
// match wins; otherwise falls back to the most-installed entry. Returns nil
// when results is empty.
func PickSkillsShExact(results []SkillsShResult, name string) *SkillsShResult {
	if len(results) == 0 {
		return nil
	}
	var best *SkillsShResult
	for i := range results {
		r := &results[i]
		if r.SkillID == name {
			return r
		}
		if best == nil || r.Installs > best.Installs {
			best = r
		}
	}
	return best
}

// InstallFromSkillsSh installs skills.sh result r into targetDir/<r.SkillID>/.
// It fetches the source repo's tarball (trying main then master), finds the
// in-tarball path of the skill folder (skills may live at arbitrary depth in
// the repo), and extracts that folder.
func InstallFromSkillsSh(r SkillsShResult, targetDir string) (*Result, error) {
	if r.SkillID == "" || r.Source == "" {
		return nil, fmt.Errorf("skills.sh result missing skillId/source")
	}
	parts := strings.SplitN(r.Source, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("skills.sh source %q is not owner/repo", r.Source)
	}
	owner, repo := parts[0], parts[1]
	// The "source" field sometimes contains a repo-internal subpath appended
	// to owner/repo (e.g. "claude-office-skills/skills"). GitHub repos only
	// have two-segment slugs, so split again and treat the rest as a prefix
	// hint we can use to disambiguate tarball probing.
	prefixHint := ""
	if idx := strings.IndexByte(repo, '/'); idx >= 0 {
		prefixHint = repo[idx+1:]
		repo = repo[:idx]
	}

	client := defaultHTTPClient()
	var lastErr error
	for _, ref := range []string{"main", "master"} {
		tarURL := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s", owner, repo, ref)

		// Probe once to discover the real in-tarball subpath of the skill
		// folder. This is cheap for small repos and avoids double-downloads
		// because the streaming tar reader bails out on the first match.
		subpath, err := findSkillDirInTarball(client, tarURL, r.SkillID)
		if err != nil {
			lastErr = err
			continue
		}
		if subpath == "" {
			// Fall back to prefix hint when the probe finds nothing but
			// skills.sh's "source" hinted at a subpath.
			if prefixHint != "" {
				subpath = prefixHint + "/" + r.SkillID
			} else {
				lastErr = fmt.Errorf("skill %q not found in %s/%s@%s", r.SkillID, owner, repo, ref)
				continue
			}
		}

		dest := fmt.Sprintf("%s/%s", strings.TrimRight(targetDir, "/"), r.SkillID)
		n, err := extractSubpath(client, tarURL, subpath, dest)
		if err != nil {
			lastErr = err
			continue
		}
		if n == 0 {
			lastErr = fmt.Errorf("extracted no files from %s (subpath %q)", tarURL, subpath)
			continue
		}
		return &Result{
			Source:     "skills.sh",
			Name:       r.SkillID,
			Version:    ref,
			InstalledAt: dest,
			FilesWritten: n,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no main or master branch on %s/%s", owner, repo)
	}
	return nil, lastErr
}
