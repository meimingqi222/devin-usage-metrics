package ext

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

type SkillPlace struct {
	Agent     model.Agent
	Dir       string
	Link      bool
	Target    string
	Shared    bool
	Inherited bool
}

type SkillRow struct {
	Name    string
	Places  []SkillPlace
	ByAgent map[model.Agent]SkillPlace
}

func InventorySkills() ([]SkillRow, error) {
	index := map[string]*SkillRow{}
	add := func(name string, p SkillPlace) {
		r, ok := index[name]
		if !ok {
			r = &SkillRow{Name: name, ByAgent: map[model.Agent]SkillPlace{}}
			index[name] = r
		}
		if p.Inherited {
			if _, on := r.ByAgent[p.Agent]; !on {
				r.ByAgent[p.Agent] = p
			}
			return
		}
		r.Places = append(r.Places, p)
		if !p.Shared {
			r.ByAgent[p.Agent] = p
		}
	}
	var errs []error
	for _, t := range matrixTargets() {
		if t.cap == CapNone {
			continue
		}
		dirs := append([]string{}, t.skillDirs...)
		dirs = append(dirs, extraSkillDirs(t)...)
		for _, dir := range dirs {
			entries, err := listSkillDirs(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.agent.Label(), err))
				continue
			}
			for _, e := range entries {
				add(e.name, SkillPlace{Agent: t.agent, Dir: e.path, Link: e.link, Target: e.target})
			}
		}
	}
	shared := sharedSkillDir()
	entries, err := listSkillDirs(shared)
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, e := range entries {
			add(e.name, SkillPlace{Dir: e.path, Link: e.link, Target: e.target, Shared: true})
		}
	}
	for _, t := range matrixTargets() {
		if t.cap == CapNone {
			continue
		}
		for _, dir := range inheritedSkillDirs(t) {
			entries, err := listSkillDirs(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.agent.Label(), err))
				continue
			}
			fromShared := samePath(dir, shared)
			for _, e := range entries {
				add(e.name, SkillPlace{Agent: t.agent, Dir: e.path, Link: e.link, Target: e.target, Shared: fromShared, Inherited: true})
			}
		}
	}
	names := make([]string, 0, len(index))
	for name := range index {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]SkillRow, 0, len(names))
	for _, name := range names {
		out = append(out, *index[name])
	}
	return out, errors.Join(errs...)
}

type skillDir struct {
	name, path, target string
	link               bool
}

func listSkillDirs(root string) ([]skillDir, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []skillDir
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(root, e.Name())
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		item := skillDir{name: e.Name(), path: p}
		if st.Mode()&os.ModeSymlink != 0 {
			// Only a link to a directory with SKILL.md is a skill; dangling
			// links and links to files would fail later in copyDir.
			if target, err := os.Stat(p); err != nil || !target.IsDir() {
				continue
			}
			item.link = true
			if t, err := os.Readlink(p); err == nil {
				item.target = t
			}
		} else if !st.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err != nil {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

// findSkill looks up one skill row. One unreadable agent directory must not
// block acting on the others, so an inventory error counts only when nothing
// was read; the caller's refresh still reports it.
func findSkill(name string) (SkillRow, bool, error) {
	rows, err := InventorySkills()
	if err != nil && len(rows) == 0 {
		return SkillRow{}, false, err
	}
	for _, r := range rows {
		if r.Name == name {
			return r, true, nil
		}
	}
	return SkillRow{}, false, nil
}

func RemoveSkill(name string, agents []model.Agent, global bool) error {
	return withLock(func() error {
		row, found, err := findSkill(name)
		if err != nil || !found {
			return err
		}
		want := map[model.Agent]bool{}
		for _, a := range agents {
			want[a] = true
		}
		var paths []string
		var remove []string
		for _, p := range row.Places {
			if p.Shared {
				if global {
					paths = append(paths, p.Dir)
					remove = append(remove, p.Dir)
				}
				continue
			}
			if global || want[p.Agent] {
				paths = append(paths, p.Dir)
				remove = append(remove, p.Dir)
			}
		}
		snaps, err := captureFiles(paths)
		if err != nil {
			return err
		}
		for _, p := range remove {
			if err := os.RemoveAll(p); err != nil {
				_ = restoreSnaps(snaps)
				return err
			}
		}
		return saveSnapshot("skill-remove", name, snaps)
	})
}

func UnshareSkill(name string) error {
	return withLock(func() error {
		row, found, err := findSkill(name)
		if err != nil || !found {
			return err
		}
		var paths []string
		for _, p := range row.Places {
			if p.Shared {
				paths = append(paths, p.Dir)
			}
		}
		if len(paths) == 0 {
			return nil
		}
		snaps, err := captureFiles(paths)
		if err != nil {
			return err
		}
		for _, p := range paths {
			if err := os.RemoveAll(p); err != nil {
				_ = restoreSnaps(snaps)
				return err
			}
		}
		return saveSnapshot("skill-unshare", name, snaps)
	})
}

type Hit struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	SkillID  string `json:"skillId"`
	Name     string `json:"name"`
	Installs int    `json:"installs"`
}

func (h Hit) OwnerRepo() (owner, repo string, ok bool) {
	parts := strings.Split(h.Source, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type searchResponse struct {
	Skills []Hit `json:"skills"`
}

var (
	searchEndpoint = "https://skills.sh/api/search"
	githubZip      = "https://codeload.github.com"
	httpDo         = http.DefaultClient
)

func SearchSkills(ctx context.Context, query string) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchEndpoint+"?q="+urlQuery(query)+"&limit=20", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := httpDo.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skills.sh search HTTP %d", res.StatusCode)
	}
	var body searchResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Skills, nil
}

func urlQuery(s string) string {
	return url.QueryEscape(s)
}

type SkillPreview struct {
	Hit         Hit
	Description string
	Markdown    string
	RelPath     string
	Files       []string
	Zip         []byte
}

func PreviewSkill(ctx context.Context, hit Hit) (SkillPreview, error) {
	ar, err := FetchSkillArchive(ctx, hit)
	if err != nil {
		return SkillPreview{Hit: hit}, err
	}
	return SkillPreview{
		Hit:         ar.Hit,
		Description: skillDescription(ar.Markdown),
		Markdown:    ar.Markdown,
		RelPath:     ar.Rel,
		Files:       ar.Files,
		Zip:         ar.Zip,
	}, nil
}

type SkillArchive struct {
	Hit      Hit
	Zip      []byte
	Rel      string
	Files    []string
	Markdown string
}

func FetchSkillArchive(ctx context.Context, hit Hit) (SkillArchive, error) {
	if err := validSkillID(hit.SkillID); err != nil {
		return SkillArchive{}, err
	}
	owner, repo, ok := hit.OwnerRepo()
	if !ok {
		return SkillArchive{}, errors.New("unsupported skill source")
	}
	zipBytes, err := fetchBytes(ctx, githubZip+"/"+owner+"/"+repo+"/zip/HEAD")
	if err != nil {
		return SkillArchive{}, err
	}
	rel, md, files, err := inspectSkillZip(zipBytes, hit.SkillID)
	if err != nil {
		return SkillArchive{}, err
	}
	return SkillArchive{Hit: hit, Zip: zipBytes, Rel: rel, Files: files, Markdown: md}, nil
}

func skillDescription(md string) string {
	body := md
	if strings.HasPrefix(body, "---") {
		rest := body[3:]
		if i := strings.Index(rest, "\n---"); i >= 0 {
			fm := rest[:i]
			body = rest[i+4:]
			for _, line := range strings.Split(fm, "\n") {
				line = strings.TrimSpace(line)
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "description" {
					return strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

var skillIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validSkillID(id string) error {
	if id == "" || id == "." || id == ".." {
		return errors.New("invalid skill id")
	}
	if strings.ContainsAny(id, `/\`) || !skillIDPattern.MatchString(id) {
		return errors.New("invalid skill id")
	}
	return nil
}

func skillDest(root, id string) (string, error) {
	if err := validSkillID(id); err != nil {
		return "", err
	}
	if root == "" {
		return "", errors.New("missing skill directory")
	}
	dest := filepath.Join(root, id)
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("invalid skill id")
	}
	return dest, nil
}

func InstallSkill(ctx context.Context, hit Hit, agents []model.Agent) error {
	ar, err := FetchSkillArchive(ctx, hit)
	if err != nil {
		return err
	}
	return InstallSkillArchive(ar, agents)
}

func InstallSkillArchive(ar SkillArchive, agents []model.Agent) error {
	if err := validSkillID(ar.Hit.SkillID); err != nil {
		return err
	}
	dir, _, err := extractSkill(ar.Zip, ar.Hit.SkillID)
	if err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Dir(dir))
	return withLock(func() error {
		var dests []string
		for _, agent := range agents {
			t, ok := targetFor(agent)
			if !ok || t.cap == CapNone || len(t.skillDirs) == 0 {
				return fmt.Errorf("%s does not support skills", agent.Label())
			}
			dest, err := skillDest(t.skillDirs[0], ar.Hit.SkillID)
			if err != nil {
				return err
			}
			dests = append(dests, dest)
		}
		snaps, err := captureFiles(dests)
		if err != nil {
			return err
		}
		for _, dest := range dests {
			if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
				_ = restoreSnaps(snaps)
				return err
			}
			_ = os.RemoveAll(dest)
			if err := copyDir(dir, dest); err != nil {
				_ = restoreSnaps(snaps)
				return err
			}
		}
		return saveSnapshot("skill-install", ar.Hit.ID, snaps)
	})
}

func EnableSkill(name string, agent model.Agent) error {
	if err := validSkillID(name); err != nil {
		return err
	}
	return withLock(func() error {
		t, ok := targetFor(agent)
		if !ok || t.cap == CapNone || len(t.skillDirs) == 0 {
			return fmt.Errorf("%s does not support skills", agent.Label())
		}
		dest, err := skillDest(t.skillDirs[0], name)
		if err != nil {
			return err
		}
		src, err := skillSourceDir(name, agent)
		if err != nil {
			return err
		}
		snaps, err := captureFiles([]string{dest})
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		_ = os.RemoveAll(dest)
		if err := copyDir(src, dest); err != nil {
			_ = restoreSnaps(snaps)
			return err
		}
		return saveSnapshot("skill-enable", name, snaps)
	})
}

func skillSourceDir(name string, dest model.Agent) (string, error) {
	row, found, err := findSkill(name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("skill not installed")
	}
	if _, on := row.ByAgent[dest]; on {
		return "", errors.New("skill already enabled")
	}
	for _, p := range row.Places {
		if p.Shared {
			return p.Dir, nil
		}
	}
	for _, p := range row.Places {
		if !p.Shared && p.Dir != "" {
			return p.Dir, nil
		}
	}
	return "", errors.New("skill not installed")
}

const (
	maxSkillFileSize = 8 << 20
	maxSkillFiles    = 200
	maxSkillTotal    = 25 << 20
)

func zipPathEscapes(name string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(name), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func skillFilePerm(mode os.FileMode, dir bool) os.FileMode {
	if dir {
		return 0700
	}
	if mode&0111 != 0 {
		return 0755
	}
	return 0600
}

func inspectSkillZip(z []byte, skillID string) (rel, md string, files []string, err error) {
	if err := validSkillID(skillID); err != nil {
		return "", "", nil, err
	}
	r, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		return "", "", nil, err
	}
	skillRel := findSkillRel(r, skillID)
	if skillRel == "" {
		return "", "", nil, errors.New("SKILL.md not found in archive")
	}
	var total int
	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		if zipPathEscapes(name) {
			return "", "", nil, errors.New("zip path escapes")
		}
		rel, ok := skillMember(name, skillRel)
		if !ok {
			continue
		}
		if rel == "" || strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if len(files) >= maxSkillFiles {
			return "", "", nil, errors.New("skill archive has too many files")
		}
		if f.UncompressedSize64 > maxSkillFileSize {
			return "", "", nil, fmt.Errorf("%s exceeds 8MiB", rel)
		}
		total += int(f.UncompressedSize64)
		if total > maxSkillTotal {
			return "", "", nil, errors.New("skill archive is too large")
		}
		files = append(files, rel)
		if md == "" && skillMarkdownName(name, skillRel) {
			rc, err := f.Open()
			if err != nil {
				return "", "", nil, err
			}
			data, err := io.ReadAll(io.LimitReader(rc, maxSkillFileSize+1))
			rc.Close()
			if err != nil {
				return "", "", nil, err
			}
			if len(data) > maxSkillFileSize {
				return "", "", nil, fmt.Errorf("%s exceeds 8MiB", rel)
			}
			md = string(data)
		}
	}
	sort.Strings(files)
	return skillRel, md, files, nil
}

// skillMember returns name relative to the skill directory skillRel; "." is
// a SKILL.md at the archive root with no wrapper directory.
func skillMember(name, skillRel string) (string, bool) {
	if skillRel == "." {
		return name, true
	}
	if name != skillRel && !strings.HasPrefix(name, skillRel+"/") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(name, skillRel), "/"), true
}

func skillMarkdownName(name, skillRel string) bool {
	name = filepath.ToSlash(name)
	if skillRel == "" || skillRel == "." {
		return strings.EqualFold(name, "SKILL.md")
	}
	return strings.EqualFold(name, skillRel+"/SKILL.md")
}

func zipRootSkillParent(parent string) bool {
	parent = path.Clean(parent)
	return parent == "." || path.Dir(parent) == "."
}

func findSkillRel(r *zip.Reader, skillID string) string {
	type hit struct{ name, parent string }
	var hits []hit
	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		if zipPathEscapes(name) {
			continue
		}
		if !strings.EqualFold(path.Base(name), "SKILL.md") {
			continue
		}
		parent := path.Dir(name)
		if strings.EqualFold(path.Base(parent), skillID) {
			return parent
		}
		hits = append(hits, hit{name, parent})
	}
	var roots []hit
	for _, h := range hits {
		if zipRootSkillParent(h.parent) {
			roots = append(roots, h)
		}
	}
	for _, h := range roots {
		md, err := zipReadLimited(r, h.name)
		if err != nil {
			continue
		}
		if strings.EqualFold(skillFrontmatterName(md), skillID) {
			return h.parent
		}
	}
	return ""
}

func zipReadLimited(r *zip.Reader, name string) (string, error) {
	for _, f := range r.File {
		if filepath.ToSlash(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxSkillFileSize+1))
		rc.Close()
		if err != nil {
			return "", err
		}
		if len(data) > maxSkillFileSize {
			return "", fmt.Errorf("%s exceeds 8MiB", name)
		}
		return string(data), nil
	}
	return "", errors.New("missing zip member")
}

func skillFrontmatterName(md string) string {
	if !strings.HasPrefix(md, "---") {
		return ""
	}
	rest := md[3:]
	i := strings.Index(rest, "\n---")
	if i < 0 {
		return ""
	}
	for _, line := range strings.Split(rest[:i], "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "name" {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func extractSkill(z []byte, skillID string) (dir, rel string, err error) {
	skillRel, _, _, err := inspectSkillZip(z, skillID)
	if err != nil {
		return "", "", err
	}
	r, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp("", "aum-skill-*")
	if err != nil {
		return "", "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(tmp)
		}
	}()
	destRoot := filepath.Join(tmp, "skill")
	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		if zipPathEscapes(name) {
			return "", "", errors.New("zip path escapes")
		}
		rel, ok := skillMember(name, skillRel)
		if !ok {
			continue
		}
		out := filepath.Join(destRoot, filepath.FromSlash(rel))
		if f.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(out, 0700); err != nil {
				return "", "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
			return "", "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", "", err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxSkillFileSize+1))
		rc.Close()
		if err != nil {
			return "", "", err
		}
		if len(data) > maxSkillFileSize {
			return "", "", fmt.Errorf("%s exceeds 8MiB", rel)
		}
		if err := os.WriteFile(out, data, skillFilePerm(f.Mode(), false)); err != nil {
			return "", "", err
		}
	}
	cleanup = false
	return destRoot, skillRel, nil
}

func copyDir(src, dest string) error {
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
			return err
		}
		return os.WriteFile(out, b, skillFilePerm(info.Mode(), false))
	})
}

func fetchBytes(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := httpDo.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 25<<20))
}
