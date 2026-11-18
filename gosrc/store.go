package main

import (
	"sort"
	"strings"
)

// In-memory store shared by REST, GraphQL, MCP and git smart-HTTP.
// Ported from store.ts; JS Map insertion order is preserved for refs and
// labels via order slices.

type Label struct {
	Name  string
	Color string
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []string
	CreatedAt string
	UpdatedAt string
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	Head           string
	Base           string
	State          string // open | closed
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	CreatedAt      string
	UpdatedAt      string
}

type Comment struct {
	ID   int
	Body string
}

type Repo struct {
	ID            int
	Name          string
	IsPrivate     bool
	Description   *string
	DefaultBranch string
	Objects       map[string]*GitObject
	Refs          map[string]string
	RefOrder      []string
	Labels        map[string]*Label
	LabelOrder    []string
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	Comments      map[int][]*Comment
	NextNumber    int
	Version       int
	CreatedAt     string
	UpdatedAt     string
}

func (r *Repo) setRef(ref, oid string) {
	if _, ok := r.Refs[ref]; !ok {
		r.RefOrder = append(r.RefOrder, ref)
	}
	r.Refs[ref] = oid
}

func (r *Repo) deleteRef(ref string) {
	if _, ok := r.Refs[ref]; !ok {
		return
	}
	delete(r.Refs, ref)
	for i, x := range r.RefOrder {
		if x == ref {
			r.RefOrder = append(r.RefOrder[:i], r.RefOrder[i+1:]...)
			break
		}
	}
}

func (r *Repo) setLabel(l *Label) {
	if _, ok := r.Labels[l.Name]; !ok {
		r.LabelOrder = append(r.LabelOrder, l.Name)
	}
	r.Labels[l.Name] = l
}

const epochSec = 1735689600 // Date.UTC(2025,0,1)

type Store struct {
	Repos       []*Repo
	ReposByName map[string]*Repo
	Viewer      *string
	NextRepoID  int
	NextIssueID int
	NextPullID  int
	NextComment int
	Tick        int64
}

func newStore() *Store {
	return &Store{
		ReposByName: map[string]*Repo{},
		NextRepoID:  700001,
		NextIssueID: 900001,
		NextPullID:  950001,
		NextComment: 800001,
	}
}

// nowIso mirrors JS: new Date(EPOCH_MS + tick*1000).toISOString() with the
// tick incremented first; times are always whole seconds.
func (s *Store) nowIso() string {
	s.Tick++
	t := epochSec + s.Tick
	return formatISO(t)
}

func (s *Store) nowSec() int64 {
	s.Tick++
	return epochSec + s.Tick
}

func formatISO(sec int64) string {
	// Manual UTC formatting to match toISOString() ("...T...Z", ms .000).
	days := sec / 86400
	rem := sec % 86400
	if rem < 0 {
		rem += 86400
		days--
	}
	h, m, ss := rem/3600, (rem%3600)/60, rem%60
	// Civil-from-days algorithm (Howard Hinnant).
	z := days + 719468
	era := z / 146097
	if z < 0 && z%146097 != 0 {
		era--
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	dd := doy - (153*mp+2)/5 + 1
	mm := mp + 3
	if mp >= 10 {
		mm = mp - 9
	}
	if mm <= 2 {
		y++
	}
	return pad4(y) + "-" + pad2(mm) + "-" + pad2(dd) + "T" + pad2(h) + ":" + pad2(m) + ":" + pad2(ss) + ".000Z"
}

func pad2(v int64) string {
	if v < 10 {
		return "0" + itoa(v)
	}
	return itoa(v)
}
func pad4(v int64) string {
	s := itoa(v)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (s *Store) viewerLogin() string {
	if s.Viewer != nil {
		return *s.Viewer
	}
	return "adaiversen"
}

func (s *Store) learnViewer(owner string) {
	if s.Viewer == nil && owner != "" {
		v := owner
		s.Viewer = &v
	}
}

func (s *Store) createRepo(name string, isPrivate bool, description *string) *Repo {
	ts := s.nowIso()
	repo := &Repo{
		ID:            s.NextRepoID,
		Name:          name,
		IsPrivate:     isPrivate,
		Description:   description,
		DefaultBranch: "main",
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		Comments:      map[int][]*Comment{},
		NextNumber:    1,
		Version:       1,
		CreatedAt:     ts,
		UpdatedAt:     ts,
	}
	s.NextRepoID++
	s.Repos = append(s.Repos, repo)
	s.ReposByName[name] = repo
	return repo
}

func (s *Store) repoByID(id int) *Repo {
	for _, r := range s.Repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s *Store) resolveRepo(owner, name string) *Repo {
	repo, ok := s.ReposByName[name]
	if !ok {
		return nil
	}
	if owner != s.viewerLogin() {
		return nil
	}
	return repo
}

func (s *Store) deleteRepo(repo *Repo) {
	out := s.Repos[:0]
	for _, r := range s.Repos {
		if r != repo {
			out = append(out, r)
		}
	}
	s.Repos = out
	delete(s.ReposByName, repo.Name)
}

// ------------------------------------------------------------ object helpers
func storeObject(repo *Repo, t ObjType, data []byte) string {
	oid := hashObject(t, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: t, Data: data}
	}
	return oid
}

func peelToCommit(repo *Repo, oid string) string {
	current := oid
	for i := 0; i < 16; i++ {
		obj, ok := repo.Objects[current]
		if !ok {
			return ""
		}
		if obj.Type == ObjCommit {
			return current
		}
		if obj.Type == ObjTag {
			current = parseTag(obj.Data).Object
			continue
		}
		return ""
	}
	return ""
}

var hex40 = func(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Commit-ish resolution for a ref name, oid, or branch name ("" = missing,
// ref "" means "use the default branch tip").
func resolveCommitish(repo *Repo, ref string, hasRef bool) string {
	if !hasRef {
		tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			return ""
		}
		return peelToCommit(repo, tip)
	}
	full := ref
	if !strings.HasPrefix(ref, "refs/") {
		full = "refs/heads/" + ref
	}
	if oid, ok := repo.Refs[full]; ok {
		return peelToCommit(repo, oid)
	}
	if hex40(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return peelToCommit(repo, ref)
		}
	}
	return ""
}

type FlatEntry struct {
	OID  string
	Mode string
}

func flattenTree(repo *Repo, treeOID string, prefix string, out map[string]FlatEntry) map[string]FlatEntry {
	if out == nil {
		out = map[string]FlatEntry{}
	}
	obj, ok := repo.Objects[treeOID]
	if !ok || obj.Type != ObjTree {
		return out
	}
	for _, entry := range parseTree(obj.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			flattenTree(repo, entry.OID, path, out)
		} else {
			out[path] = FlatEntry{OID: entry.OID, Mode: entry.Mode}
		}
	}
	return out
}

func treeOfCommit(repo *Repo, commitOID string) string {
	obj, ok := repo.Objects[commitOID]
	if !ok || obj.Type != ObjCommit {
		return ""
	}
	return commitHeaderOf(obj).Tree
}

func flattenCommitTree(repo *Repo, commitOID string) map[string]FlatEntry {
	tree := treeOfCommit(repo, commitOID)
	if tree == "" {
		return map[string]FlatEntry{}
	}
	return flattenTree(repo, tree, "", nil)
}

func buildTreeFromFlat(repo *Repo, flat map[string]FlatEntry) string {
	type node struct {
		files map[string]FlatEntry
		dirs  map[string]*node
	}
	newNode := func() *node { return &node{files: map[string]FlatEntry{}, dirs: map[string]*node{}} }
	root := newNode()
	for path, value := range flat {
		parts := strings.Split(path, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			next, ok := cur.dirs[parts[i]]
			if !ok {
				next = newNode()
				cur.dirs[parts[i]] = next
			}
			cur = next
		}
		cur.files[parts[len(parts)-1]] = value
	}
	var emit func(n *node) string
	emit = func(n *node) string {
		var entries []TreeEntry
		for name, value := range n.files {
			entries = append(entries, TreeEntry{Mode: value.Mode, Name: name, OID: value.OID})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: emit(child)})
		}
		return storeObject(repo, ObjTree, buildTreeData(entries))
	}
	return emit(root)
}

func makeCommit(store *Store, repo *Repo, tree string, parents []string, message string) string {
	login := store.viewerLogin()
	data := buildCommitData(tree, parents, login, login+"@example.invalid", login, login+"@example.invalid", store.nowSec(), message)
	return storeObject(repo, ObjCommit, data)
}

// Apply a file change (set or delete a path) on a branch; returns new commit.
func commitFileChange(store *Store, repo *Repo, branch, path string, blobOID *string, message string) string {
	ref := "refs/heads/" + branch
	tip, hasTip := repo.Refs[ref]
	tipCommit := ""
	if hasTip {
		tipCommit = peelToCommit(repo, tip)
	}
	var flat map[string]FlatEntry
	if tipCommit != "" {
		flat = flattenCommitTree(repo, tipCommit)
	} else {
		flat = map[string]FlatEntry{}
	}
	if blobOID == nil {
		delete(flat, path)
	} else {
		flat[path] = FlatEntry{OID: *blobOID, Mode: "100644"}
	}
	tree := buildTreeFromFlat(repo, flat)
	var parents []string
	if tipCommit != "" {
		parents = []string{tipCommit}
	}
	commit := makeCommit(store, repo, tree, parents, message)
	repo.setRef(ref, commit)
	repo.Version++
	repo.UpdatedAt = store.nowIso()
	return commit
}

// ------------------------------------------------------------- reachability
func ancestorDistances(repo *Repo, start string, limit int) map[string]int {
	dist := map[string]int{}
	startCommit := peelToCommit(repo, start)
	if startCommit == "" {
		return dist
	}
	queue := []string{startCommit}
	dist[startCommit] = 0
	for head := 0; head < len(queue) && len(dist) < limit; head++ {
		current := queue[head]
		d := dist[current]
		obj, ok := repo.Objects[current]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, parent := range commitHeaderOf(obj).Parents {
			if _, seen := dist[parent]; !seen {
				dist[parent] = d + 1
				queue = append(queue, parent)
			}
		}
	}
	return dist
}

func isAncestor(repo *Repo, ancestor, descendant string) bool {
	a := peelToCommit(repo, ancestor)
	d := peelToCommit(repo, descendant)
	if a == "" || d == "" {
		return false
	}
	if a == d {
		return true
	}
	seen := map[string]bool{d: true}
	stack := []string{d}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj, ok := repo.Objects[current]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, parent := range commitHeaderOf(obj).Parents {
			if parent == a {
				return true
			}
			if !seen[parent] {
				seen[parent] = true
				stack = append(stack, parent)
			}
		}
	}
	return false
}

func mergeBase(repo *Repo, a, b string) string {
	ca := peelToCommit(repo, a)
	cb := peelToCommit(repo, b)
	if ca == "" || cb == "" {
		return ""
	}
	ancestorsA := ancestorDistances(repo, ca, 100000)
	seen := map[string]bool{cb: true}
	queue := []string{cb}
	for head := 0; head < len(queue); head++ {
		current := queue[head]
		if _, ok := ancestorsA[current]; ok {
			return current
		}
		obj, ok := repo.Objects[current]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, parent := range commitHeaderOf(obj).Parents {
			if !seen[parent] {
				seen[parent] = true
				queue = append(queue, parent)
			}
		}
	}
	return ""
}

// Ordered history (newest first) from a commit tip.
func commitHistory(repo *Repo, tip string, limit int) []string {
	start := peelToCommit(repo, tip)
	if start == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 && len(out) < limit {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[current] {
			continue
		}
		seen[current] = true
		obj, ok := repo.Objects[current]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		out = append(out, current)
		parents := commitHeaderOf(obj).Parents
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	return out
}

func collectClosure(repo *Repo, roots []string) map[string]bool {
	out := map[string]bool{}
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		obj, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		out[oid] = true
		switch obj.Type {
		case ObjCommit:
			h := commitHeaderOf(obj)
			stack = append(stack, h.Tree)
			stack = append(stack, h.Parents...)
		case ObjTree:
			for _, entry := range parseTree(obj.Data) {
				stack = append(stack, entry.OID)
			}
		case ObjTag:
			stack = append(stack, parseTag(obj.Data).Object)
		}
	}
	return out
}

// --------------------------------------------------------------- pull merges
type FileChange struct {
	Filename  string
	Status    string // added | removed | modified
	Additions int
	Deletions int
	Changes   int
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return strings.Count(string(data), "\n") + 1
}

func pullChanges(repo *Repo, pull *Pull) []FileChange {
	baseTip, okB := repo.Refs["refs/heads/"+pull.Base]
	headTip, okH := repo.Refs["refs/heads/"+pull.Head]
	if !okB || !okH {
		return nil
	}
	mb := mergeBase(repo, baseTip, headTip)
	if mb == "" {
		mb = baseTip
	}
	fromFlat := flattenCommitTree(repo, mb)
	toFlat := flattenCommitTree(repo, headTip)
	pathSet := map[string]bool{}
	for p := range fromFlat {
		pathSet[p] = true
	}
	for p := range toFlat {
		pathSet[p] = true
	}
	var paths []string
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changes []FileChange
	for _, path := range paths {
		from, hasFrom := fromFlat[path]
		to, hasTo := toFlat[path]
		if hasFrom && hasTo && from.OID == to.OID {
			continue
		}
		additions, deletions := 0, 0
		if hasTo {
			if obj, ok := repo.Objects[to.OID]; ok {
				additions = lineCount(obj.Data)
			}
		}
		if hasFrom {
			if obj, ok := repo.Objects[from.OID]; ok {
				deletions = lineCount(obj.Data)
			}
		}
		status := "modified"
		if !hasFrom {
			status = "added"
		} else if !hasTo {
			status = "removed"
		}
		changes = append(changes, FileChange{Filename: path, Status: status, Additions: additions, Deletions: deletions, Changes: additions + deletions})
	}
	return changes
}

type mergeComputation struct {
	Conflict bool
	Tree     string
	BaseTip  string
	HeadTip  string
}

func computeMerge(repo *Repo, pull *Pull) *mergeComputation {
	baseTip, okB := repo.Refs["refs/heads/"+pull.Base]
	headTip, okH := repo.Refs["refs/heads/"+pull.Head]
	if !okB || !okH {
		return nil
	}
	mb := mergeBase(repo, baseTip, headTip)
	if mb == "" {
		return nil
	}
	mbFlat := flattenCommitTree(repo, mb)
	baseFlat := flattenCommitTree(repo, baseTip)
	headFlat := flattenCommitTree(repo, headTip)
	resultFlat := map[string]FlatEntry{}
	for k, v := range baseFlat {
		resultFlat[k] = v
	}
	conflict := false
	pathSet := map[string]bool{}
	for p := range mbFlat {
		pathSet[p] = true
	}
	for p := range headFlat {
		pathSet[p] = true
	}
	for path := range pathSet {
		mbEntry, hasMb := mbFlat[path]
		headEntry, hasHead := headFlat[path]
		if hasMb && hasHead && mbEntry.OID == headEntry.OID {
			continue
		}
		if !hasMb && !hasHead {
			continue
		}
		// Mirror the JS null comparisons: (base?.oid ?? null) !== (mb?.oid ?? null),
		// with "" standing in for null (git oids are never empty).
		baseChanged := nullableOID(baseFlat, path) != nullableOID(mbFlat, path)
		sameAsHead := nullableOID(baseFlat, path) == nullableOID(headFlat, path)
		if baseChanged && !sameAsHead {
			conflict = true
			continue
		}
		if hasHead {
			resultFlat[path] = headEntry
		} else {
			delete(resultFlat, path)
		}
	}
	tree := buildTreeFromFlat(repo, resultFlat)
	return &mergeComputation{Conflict: conflict, Tree: tree, BaseTip: baseTip, HeadTip: headTip}
}

// nullableOID returns (oid, present) collapsed into a comparable pair:
// we encode missing as "" with a flag via a string sentinel — git oids are
// 40-char hex, never empty, so "" works as null.
func nullableOID(m map[string]FlatEntry, p string) string {
	if e, ok := m[p]; ok {
		return e.OID
	}
	return ""
}

// mergePull returns (commitOID, outcome) where outcome is "", "conflict",
// or "null" (cannot merge).
func mergePull(store *Store, repo *Repo, pull *Pull, method string) (string, string) {
	if pull.Merged || pull.State == "closed" {
		return "", "null"
	}
	computed := computeMerge(repo, pull)
	if computed == nil {
		return "", "null"
	}
	if computed.Conflict {
		return "", "conflict"
	}
	baseCommit := peelToCommit(repo, computed.BaseTip)
	headCommit := peelToCommit(repo, computed.HeadTip)
	if baseCommit == "" || headCommit == "" {
		return "", "null"
	}
	var commit string
	if method == "merge" {
		commit = makeCommit(store, repo, computed.Tree, []string{baseCommit, headCommit},
			"Merge pull request #"+itoa(int64(pull.Number))+" from "+pull.Head+"\n\n"+pull.Title)
	} else {
		body := ""
		if pull.Body != "" {
			body = "\n\n" + pull.Body
		}
		commit = makeCommit(store, repo, computed.Tree, []string{baseCommit},
			pull.Title+" (#"+itoa(int64(pull.Number))+")"+body)
	}
	repo.setRef("refs/heads/"+pull.Base, commit)
	repo.Version++
	repo.UpdatedAt = store.nowIso()
	pull.State = "closed"
	pull.Merged = true
	mergedAt := store.nowIso()
	pull.MergedAt = &mergedAt
	pull.MergeCommitSha = &commit
	pull.UpdatedAt = store.nowIso()
	return commit, ""
}

// -------------------------------------------------------------- global ids
func globalId(t string, parts ...interface{}) string {
	var sb strings.Builder
	sb.WriteString(t)
	for _, p := range parts {
		sb.WriteByte(':')
		switch v := p.(type) {
		case string:
			sb.WriteString(v)
		case int:
			sb.WriteString(itoa(int64(v)))
		case int64:
			sb.WriteString(itoa(v))
		}
	}
	return base64Encode([]byte(sb.String()))
}

type decodedGlobalID struct {
	Type  string
	Parts []string
}

func decodeGlobalId(id string) *decodedGlobalID {
	text := string(lenientBase64Decode(id))
	parts := strings.Split(text, ":")
	if len(parts) < 2 {
		return nil
	}
	return &decodedGlobalID{Type: parts[0], Parts: parts[1:]}
}
