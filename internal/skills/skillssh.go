package skills

import (
	"encoding/json"
	"fmt"
	"io"
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
	ID       string `json:"id"`                // "<owner>/<repo>/<skillId>" (display-only)
	SkillID  string `json:"skillId"`           // folder name of the skill inside the source repo
	Name     string `json:"name"`              // human-readable name
	Source   string `json:"source"`            // "<owner>/<repo>" — the GitHub location
	Installs int    `json:"installs"`          // popularity hint for ranking
	Version  string `json:"version,omitempty"` // latest release tag (GitHub API)
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
	// Version enrichment is intentionally NOT done here for search results.
	// GitHub API rate limit (60 req/h unauthenticated) makes it infeasible
	// to call /releases/latest per result for 100-result pages. Version is
	// resolved at install time instead (SKILL.md → GitHub Release → ref).
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

// PickSkillsShBySource returns the result matching both skillId and Source
// (owner/repo). Returns nil if no match — caller should fall back to
// PickSkillsShExact or direct GitHub install.
func PickSkillsShBySource(results []SkillsShResult, skillID, ownerRepo string) *SkillsShResult {
	for i := range results {
		r := &results[i]
		if r.SkillID == skillID && strings.EqualFold(r.Source, ownerRepo) {
			return r
		}
	}
	return nil
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

	// Resolve the canonical repo identity first — one GitHub API call that
	// (a) follows rename redirects (a repo may have been renamed since
	// skills.sh indexed its source; codeload does NOT follow renames, so
	// the tarball URL MUST be built from the canonical name), and (b) also
	// returns the default branch. On API failure we fall back to the input
	// owner/repo and the well-known branches without an extra API call.
	canonicalSource := r.Source
	refs := []string{"main", "master"}
	if co, cr, def, ok := githubRepoIdentity(client, owner, repo); ok {
		owner, repo = co, cr
		canonicalSource = fmt.Sprintf("%s/%s", co, cr)
		// Try the repo's actual default branch first, then the common
		// conventions as fallback. Many skills.sh entries point at repos
		// with non-standard branches (e.g. `trunk`, `develop`, `dev`).
		// Dedup so we don't hit the same ref twice when default is already
		// `main`/`master`.
		if def != "" {
			if def != "main" && def != "master" {
				refs = append([]string{def}, refs...)
			} else {
				// Move matching ref to front to short-circuit the happy path.
				refs = append([]string{def}, filterOut(refs, def)...)
			}
		}
	}
	var lastErr error
	for _, ref := range refs {
		tarURL := codeloadTarURL(owner, repo, ref)

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
			} else if repoIsSkill, _ := repoHasRootSkillMD(client, tarURL); repoIsSkill {
				// Repo itself IS the skill (SKILL.md at root, no subfolder).
				// e.g. tokenaissance/silicon-economics-skill where the
				// skillId doesn't appear as a subfolder in the tarball.
				subpath = ""
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
		version := readSkillVersionFromDir(dest)
		if version == "" {
			version = latestGitHubRelease(client, owner, repo)
		}
		if version == "" {
			version = ref
		}
		writeInstallMetadata(dest, canonicalSource)
		return &Result{
			Source:       "skills.sh",
			Repo:         canonicalSource,
			Name:         r.SkillID,
			Version:      version,
			InstalledAt:  dest,
			FilesWritten: n,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no main or master branch on %s/%s", owner, repo)
	}
	return nil, lastErr
}

// githubRepoIdentity resolves the canonical owner/repo and default branch of a
// GitHub repository via the GitHub API. api.github.com returns a 301 that
// net/http follows transparently when a repo has been renamed, so the
// response's full_name reflects the CURRENT repo — whereas codeload.github.com
// does NOT follow renames, so installs that build tarball URLs from a stale
// name 404 even though the API resolves it. Returns ("", "", "", false) on any
// error (private repo, rate limit, etc.) so callers fall back to the input
// owner/repo and the well-known branches. Best-effort only; never blocks.
func githubRepoIdentity(client *http.Client, owner, repo string) (canonicalOwner, canonicalRepo, defaultBranch string, ok bool) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", "", "", false
	}
	// Explicit Accept keeps the v3 JSON format stable. No auth header —
	// unauthenticated requests have a low rate limit (60/h per IP) but
	// that's enough for interactive installs and we don't want to require
	// configuring a token just for repo-identity lookup.
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", false
	}
	var body struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", "", false
	}
	if co, cr, valid := SplitOwnerRepo(body.FullName); valid {
		return co, cr, body.DefaultBranch, true
	}
	return "", "", "", false
}

// githubDefaultBranch asks the GitHub API for the repo's default branch via
// githubRepoIdentity (which follows rename redirects). Returns "" on any error
// — callers fall back to the well-known conventions. Best-effort only.
func githubDefaultBranch(client *http.Client, owner, repo string) string {
	_, _, def, ok := githubRepoIdentity(client, owner, repo)
	if !ok {
		return ""
	}
	return def
}

// ProbeGitHubRepo checks whether a public GitHub repo matching owner/repo
// exposes any discoverable SKILL.md entrypoints. It returns skills.sh-shaped
// results so callers can treat GitHub-discovered skills identically in search
// and install flows. Returns (nil, nil) when the repo exists but no
// discoverable skill is found.
//
// Discovery strategy:
//  1. Verify the repo exists (200 from GitHub API).
//  2. Check for SKILL.md at repo root — the common "repo is the skill" case
//     (e.g. tokenaissance/fastagent-meta-skill).
//  3. Otherwise return nil.
func ProbeGitHubRepo(owner, repo string) ([]SkillsShResult, error) {
	client := defaultHTTPClient()

	// Step 1 — verify the repo exists and get its default branch + stars.
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe GitHub repo %s/%s: %w", owner, repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Not a public repo — return empty, not an error.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("probe GitHub repo %s/%s: HTTP %d", owner, repo, resp.StatusCode)
	}
	var repoInfo struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Stars         int    `json:"stargazers_count"`
		Description   string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&repoInfo); err != nil {
		return nil, fmt.Errorf("decode GitHub repo info: %w", err)
	}

	// Step 2 — check for SKILL.md at repo root.
	contentsURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/SKILL.md", owner, repo)
	req2, _ := http.NewRequest(http.MethodGet, contentsURL, nil)
	req2.Header.Set("Accept", "application/vnd.github+json")
	resp2, err := client.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("probe SKILL.md for %s/%s: %w", owner, repo, err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode == http.StatusOK {
		// Repo root IS a skill — return a skills.sh-shaped result.
		version := latestGitHubRelease(client, owner, repo)
		return []SkillsShResult{{
			ID:       fmt.Sprintf("%s/%s/%s", owner, repo, repo),
			SkillID:  repo,
			Name:     repo,
			Source:   fmt.Sprintf("%s/%s", owner, repo),
			Installs: 0,
			Version:  version,
		}}, nil
	}

	// No discoverable entrypoint found.
	return nil, nil
}

// splitOwnerRepo returns (owner, repo, true) when s looks like "owner/repo"
// (exactly one slash, non-empty parts, no @ prefix / trailing path / extra
// slashes / URL scheme). Returns ("", "", false) for anything else.
// latestGitHubRelease returns the tag name of the latest GitHub Release for
// owner/repo, or "" on any error (private, no releases, rate limit, etc.).
func latestGitHubRelease(client *http.Client, owner, repo string) string {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return strings.TrimPrefix(body.TagName, "v")
}

func SplitOwnerRepo(s string) (string, string, bool) {
	s = strings.TrimSpace(s)
	// Reject URLs, @skill suffixes, and subpaths beyond owner/repo.
	if strings.Contains(s, "://") || strings.Contains(s, "@") || strings.HasPrefix(s, "/") {
		return "", "", false
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func filterOut(items []string, drop string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// SortByQueryPrefix re-ranks search results so skills whose skillId matches
// the query (exact, then prefix) appear first, then by installs descending.
// This ensures low-install exact matches (e.g. "silicon-economics" with 1
// install) aren't buried below high-install fuzzy matches.
func SortByQueryPrefix(results []SkillsShResult, query string) {
	if len(results) <= 1 || query == "" {
		return
	}
	qlower := strings.ToLower(strings.TrimSpace(query))

	// Score each result: 3 = exact skillId, 2 = skillId prefix, 1 = name
	// prefix, 0 = rest. Sort by score desc, then installs desc.
	score := func(r *SkillsShResult) int {
		sid := strings.ToLower(r.SkillID)
		if sid == qlower {
			return 3
		}
		if strings.HasPrefix(sid, qlower) {
			return 2
		}
		if strings.HasPrefix(strings.ToLower(r.Name), qlower) {
			return 1
		}
		return 0
	}

	// Stable sort: group into buckets so non-matching order (installs
	// desc within each bucket) is preserved from skills.sh's own ranking.
	// Use insertion-style partition since n ≤ 100.
	type entry struct {
		r     SkillsShResult
		score int
	}
	entries := make([]entry, len(results))
	for i := range results {
		entries[i] = entry{r: results[i], score: score(&results[i])}
	}
	// Sort by score descending, tie-break by installs descending.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0; j-- {
			if entries[j].score > entries[j-1].score ||
				(entries[j].score == entries[j-1].score && entries[j].r.Installs > entries[j-1].r.Installs) {
				entries[j], entries[j-1] = entries[j-1], entries[j]
			} else {
				break
			}
		}
	}
	for i := range entries {
		results[i] = entries[i].r
	}
}

// FetchSkillReadme fetches SKILL.md from a GitHub repo and returns the body
// without YAML frontmatter for display on skill detail pages. Probes multiple
// paths: repo root (repo-is-the-skill), skills/{name}/, {name}/.
func FetchSkillReadme(owner, repo, skillName string) (string, error) {
	client := defaultHTTPClient()
	ref := githubDefaultBranch(client, owner, repo)
	if ref == "" {
		ref = "main"
	}

	// Strategy 1: SKILL.md at repo root (repo IS the skill).
	if content := fetchGitHubFileContent(owner, repo, ref, "SKILL.md"); content != "" {
		return stripFrontmatterBody(content), nil
	}

	// Strategy 2: skills/{name}/SKILL.md.
	path := fmt.Sprintf("skills/%s/SKILL.md", skillName)
	if content := fetchGitHubFileContent(owner, repo, ref, path); content != "" {
		return stripFrontmatterBody(content), nil
	}

	// Strategy 3: {name}/SKILL.md.
	path = fmt.Sprintf("%s/SKILL.md", skillName)
	if content := fetchGitHubFileContent(owner, repo, ref, path); content != "" {
		return stripFrontmatterBody(content), nil
	}

	return "", nil
}

// fetchGitHubFileContent fetches a single file from raw.githubusercontent.com.
// Returns "" on any error (not found, rate limit, etc.).
func fetchGitHubFileContent(owner, repo, ref, path string) string {
	u := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, ref, path)
	resp, err := defaultHTTPClient().Get(u)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return ""
	}
	return string(body)
}

// stripFrontmatterBody strips YAML frontmatter (--- ... ---) from SKILL.md
// content, returning only the body. Returns raw content on parse failure.
func stripFrontmatterBody(content string) string {
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, "---") {
		return s
	}
	rest := s[3:]
	endIdx := strings.Index(rest, "\n---")
	if endIdx < 0 {
		return s
	}
	body := strings.TrimLeft(rest[endIdx+len("\n---"):], "\n")
	if body == "" {
		return s // frontmatter-only: return raw
	}
	return body
}
