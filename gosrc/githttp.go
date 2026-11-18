package main

import (
	"bytes"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Git smart HTTP (ported from git-http.ts).

const agentString = "forge/1.0"

func sortedRefs(repo *Repo) [][2]string {
	refs := make([][2]string, 0, len(repo.Refs))
	for _, name := range repo.RefOrder {
		refs = append(refs, [2]string{name, repo.Refs[name]})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i][0] < refs[j][0] })
	return refs
}

func peeledOID(repo *Repo, oid string) string {
	obj, ok := repo.Objects[oid]
	if !ok || obj.Type != ObjTag {
		return ""
	}
	target := parseTag(obj.Data).Object
	if _, ok := repo.Objects[target]; ok {
		return target
	}
	return ""
}

func sidebandPackets(pack []byte) [][]byte {
	var out [][]byte
	const chunk = 32768
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		msg := make([]byte, 0, end-offset+1)
		msg = append(msg, 1)
		msg = append(msg, pack[offset:end]...)
		out = append(out, pktLine(msg))
	}
	if len(out) == 0 {
		out = append(out, pktLine([]byte{1}))
	}
	return out
}

// Closure that includes trees/blobs fully but does not expand the parents
// of boundary commits.
func boundedClosure(repo *Repo, roots []string, boundary map[string]bool) map[string]bool {
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
			if !boundary[oid] {
				stack = append(stack, h.Parents...)
			}
		case ObjTree:
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		case ObjTag:
			stack = append(stack, parseTag(obj.Data).Object)
		}
	}
	return out
}

func addTreeClosure(repo *Repo, treeOID string, out map[string]bool) {
	stack := []string{treeOID}
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
		if obj.Type == ObjTree {
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.OID)
			}
		}
	}
}

type fetchPlan struct {
	objects   []string
	shallow   []string
	unshallow []string
}

func planFetch(repo *Repo, wants, haves, clientShallow []string, depth *int) fetchPlan {
	shallowSet := map[string]bool{}
	for _, s := range clientShallow {
		shallowSet[s] = true
	}
	heldCommits := map[string]bool{}
	{
		var stack []string
		for _, h := range haves {
			if c := peelToCommit(repo, h); c != "" {
				stack = append(stack, c)
			}
		}
		seen := map[string]bool{}
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[current] {
				continue
			}
			seen[current] = true
			heldCommits[current] = true
			if shallowSet[current] {
				continue
			}
			if obj, ok := repo.Objects[current]; ok && obj.Type == ObjCommit {
				stack = append(stack, commitHeaderOf(obj).Parents...)
			}
		}
	}
	var heldRoots []string
	for c := range heldCommits {
		heldRoots = append(heldRoots, c)
	}
	heldObjects := boundedClosure(repo, heldRoots, shallowSet)

	if depth == nil {
		wantClosure := collectClosure(repo, wants)
		var objects []string
		for oid := range wantClosure {
			if !heldObjects[oid] {
				objects = append(objects, oid)
			}
		}
		included := map[string]bool{}
		for c := range heldCommits {
			included[c] = true
		}
		for _, oid := range objects {
			if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjCommit {
				included[oid] = true
			}
		}
		var unshallow []string
		for _, oid := range clientShallow {
			if !included[oid] {
				continue
			}
			if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjCommit {
				parents := commitHeaderOf(obj).Parents
				all := true
				for _, p := range parents {
					if !included[p] {
						all = false
						break
					}
				}
				if all {
					unshallow = append(unshallow, oid)
				}
			}
		}
		return fetchPlan{objects: objects, unshallow: unshallow}
	}

	// Depth-limited: BFS by generation from the wants.
	d := *depth
	generations := map[string]int{}
	type qitem struct {
		oid string
		gen int
	}
	var queue []qitem
	for _, want := range wants {
		commit := peelToCommit(repo, want)
		if commit != "" {
			if _, ok := generations[commit]; !ok {
				generations[commit] = 1
				queue = append(queue, qitem{commit, 1})
			}
		}
	}
	for head := 0; head < len(queue); head++ {
		item := queue[head]
		if item.gen >= d {
			continue
		}
		obj, ok := repo.Objects[item.oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, parent := range commitHeaderOf(obj).Parents {
			if _, ok := generations[parent]; !ok {
				generations[parent] = item.gen + 1
				queue = append(queue, qitem{parent, item.gen + 1})
			}
		}
	}
	sendCommits := map[string]bool{}
	for c := range generations {
		sendCommits[c] = true
	}
	sendSet := map[string]bool{}
	for _, want := range wants {
		current := want
		for current != "" {
			obj, ok := repo.Objects[current]
			if !ok || obj.Type != ObjTag {
				break
			}
			sendSet[current] = true
			current = parseTag(obj.Data).Object
		}
	}
	for commit := range sendCommits {
		sendSet[commit] = true
		if tree := treeOfCommit(repo, commit); tree != "" {
			addTreeClosure(repo, tree, sendSet)
		}
	}
	var objects []string
	for oid := range sendSet {
		if !heldObjects[oid] {
			objects = append(objects, oid)
		}
	}
	included := map[string]bool{}
	for c := range heldCommits {
		included[c] = true
	}
	for c := range sendCommits {
		included[c] = true
	}
	boundary := map[string]bool{}
	for oid, gen := range generations {
		if gen != d {
			continue
		}
		if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjCommit && len(commitHeaderOf(obj).Parents) > 0 {
			boundary[oid] = true
		}
	}
	var shallow []string
	for oid := range boundary {
		if !shallowSet[oid] {
			shallow = append(shallow, oid)
		}
	}
	var unshallow []string
	for _, oid := range clientShallow {
		if !included[oid] || boundary[oid] {
			continue
		}
		if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjCommit {
			parents := commitHeaderOf(obj).Parents
			all := len(parents) == 0
			if !all {
				all = true
				for _, p := range parents {
					if !included[p] {
						all = false
						break
					}
				}
			}
			if all {
				unshallow = append(unshallow, oid)
			}
		}
	}
	return fetchPlan{objects: objects, shallow: shallow, unshallow: unshallow}
}

var packCache sync.Map // string -> []byte
var packCacheCount int64
var packCacheMu sync.Mutex

// packBuildMu serializes cache-miss builds (singleflight): the clone
// bench fires 50 concurrent identical fetches, and without it every one
// of them builds the same pack before the first store lands.
var packBuildMu sync.Mutex

func buildPack(repo *Repo, objectOids []string) []byte {
	sorted := append([]string{}, objectOids...)
	sort.Strings(sorted)
	key := strconv.Itoa(repo.ID) + ":" + strconv.Itoa(repo.Version) + ":" + strings.Join(sorted, ",")
	if cached, ok := packCache.Load(key); ok {
		return cached.([]byte)
	}
	packBuildMu.Lock()
	defer packBuildMu.Unlock()
	if cached, ok := packCache.Load(key); ok {
		return cached.([]byte)
	}
	var entries []packEntry
	for _, oid := range objectOids {
		if obj, ok := repo.Objects[oid]; ok {
			entries = append(entries, packEntry{OID: oid, Type: obj.Type, Data: obj.Data})
		}
	}
	pack := writePack(entries)
	packCacheMu.Lock()
	packCacheCount++
	if packCacheCount > 128 {
		packCache.Range(func(k, v interface{}) bool { packCache.Delete(k); return true })
		packCacheCount = 1
	}
	packCacheMu.Unlock()
	packCache.Store(key, pack)
	return pack
}

// ------------------------------------------------------------- advertisement
func refsAdvertisement(repo *Repo, capabilities string) []byte {
	refs := sortedRefs(repo)
	headRef := "refs/heads/" + repo.DefaultBranch
	headOID, hasHead := repo.Refs[headRef]
	var buf bytes.Buffer
	if len(refs) == 0 {
		buf.Write(pktLineStr(ZeroOID + " capabilities^{}\x00" + capabilities + "\n"))
		buf.Write(pktFlush)
		return buf.Bytes()
	}
	first := true
	if hasHead {
		buf.Write(pktLineStr(headOID + " HEAD\x00" + capabilities + "\n"))
		first = false
	}
	for _, pair := range refs {
		ref, oid := pair[0], pair[1]
		if first {
			buf.Write(pktLineStr(oid + " " + ref + "\x00" + capabilities + "\n"))
		} else {
			buf.Write(pktLineStr(oid + " " + ref + "\n"))
		}
		first = false
		if peeled := peeledOID(repo, oid); peeled != "" {
			buf.Write(pktLineStr(peeled + " " + ref + "^{}\n"))
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

func advertisement(ctx *Ctx, repo *Repo, service string) Reply {
	isV2 := strings.Contains(ctx.header("git-protocol"), "version=2")
	if service == "git-upload-pack" && isV2 {
		var buf bytes.Buffer
		buf.Write(pktLineStr("version 2\n"))
		buf.Write(pktLineStr("agent=" + agentString + "\n"))
		buf.Write(pktLineStr("ls-refs=unborn\n"))
		buf.Write(pktLineStr("fetch=shallow\n"))
		buf.Write(pktLineStr("object-format=sha1\n"))
		buf.Write(pktFlush)
		reply := bodyReply(200, "application/x-git-upload-pack-advertisement", buf.Bytes())
		reply.Headers["cache-control"] = "no-cache"
		return reply
	}
	var capabilities string
	if service == "git-upload-pack" {
		capabilities = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not agent=" + agentString + " symref=HEAD:refs/heads/" + repo.DefaultBranch
	} else {
		capabilities = "report-status side-band-64k delete-refs ofs-delta agent=" + agentString
	}
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=" + service + "\n"))
	buf.Write(pktFlush)
	buf.Write(refsAdvertisement(repo, capabilities))
	kind := "upload-pack"
	if service != "git-upload-pack" {
		kind = "receive-pack"
	}
	reply := bodyReply(200, "application/x-git-"+kind+"-advertisement", buf.Bytes())
	reply.Headers["cache-control"] = "no-cache"
	return reply
}

// ------------------------------------------------------------------ ls-refs
func lsRefs(repo *Repo, args []string) Reply {
	peel := containsStr(args, "peel")
	symrefs := containsStr(args, "symrefs")
	unborn := containsStr(args, "unborn")
	var prefixes []string
	for _, a := range args {
		if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	headRef := "refs/heads/" + repo.DefaultBranch
	headOID, hasHead := repo.Refs[headRef]
	headMatches := len(prefixes) == 0
	if !headMatches {
		for _, p := range prefixes {
			if strings.HasPrefix("HEAD", p) {
				headMatches = true
				break
			}
		}
	}
	if headMatches {
		if hasHead {
			if symrefs {
				buf.Write(pktLineStr(headOID + " HEAD symref-target:" + headRef + "\n"))
			} else {
				buf.Write(pktLineStr(headOID + " HEAD\n"))
			}
		} else if unborn {
			buf.Write(pktLineStr("unborn HEAD symref-target:" + headRef + "\n"))
		} else {
			buf.Write(pktLineStr(ZeroOID + " HEAD symref-target:" + headRef + "\n"))
		}
	}
	for _, pair := range sortedRefs(repo) {
		ref, oid := pair[0], pair[1]
		if !matches(ref) {
			continue
		}
		line := oid + " " + ref
		if peel {
			if peeled := peeledOID(repo, oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		buf.Write(pktLineStr(line + "\n"))
	}
	buf.Write(pktFlush)
	return bodyReply(200, "application/x-git-upload-pack-result", buf.Bytes())
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------- fetch
func fetchReply(repo *Repo, wants, haves, clientShallow []string, depth *int, v2 bool, done bool, deepenRelative bool) Reply {
	if depth != nil && deepenRelative && len(clientShallow) > 0 {
		current := 0
		for _, want := range wants {
			distances := ancestorDistances(repo, want, 100000)
			for _, boundary := range clientShallow {
				commit := peelToCommit(repo, boundary)
				if commit != "" {
					if d, ok := distances[commit]; ok && d+1 > current {
						current = d + 1
					}
				}
			}
		}
		if current > 0 {
			nd := *depth + current
			depth = &nd
		}
	}
	plan := planFetch(repo, wants, haves, clientShallow, depth)
	pack := buildPack(repo, plan.objects)
	band := sidebandPackets(pack)
	var buf bytes.Buffer
	if v2 {
		if !done {
			buf.Write(pktLineStr("acknowledgments\n"))
			var present []string
			for _, h := range haves {
				if _, ok := repo.Objects[h]; ok {
					present = append(present, h)
				}
			}
			if len(present) == 0 {
				buf.Write(pktLineStr("NAK\n"))
			} else {
				for _, have := range present {
					buf.Write(pktLineStr("ACK " + have + "\n"))
				}
			}
			buf.Write(pktFlush)
			return bodyReply(200, "application/x-git-upload-pack-result", buf.Bytes())
		}
		if depth != nil || len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
			buf.Write(pktLineStr("shallow-info\n"))
			for _, oid := range plan.shallow {
				buf.Write(pktLineStr("shallow " + oid + "\n"))
			}
			for _, oid := range plan.unshallow {
				buf.Write(pktLineStr("unshallow " + oid + "\n"))
			}
			buf.Write(pktFlush)
		}
		buf.Write(pktLineStr("packfile\n"))
		for _, b := range band {
			buf.Write(b)
		}
		buf.Write(pktFlush)
	} else {
		for _, oid := range plan.shallow {
			buf.Write(pktLineStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			buf.Write(pktLineStr("unshallow " + oid + "\n"))
		}
		var present []string
		for _, h := range haves {
			if _, ok := repo.Objects[h]; ok {
				present = append(present, h)
			}
		}
		if len(present) == 0 {
			buf.Write(pktLineStr("NAK\n"))
		} else {
			buf.Write(pktLineStr("ACK " + present[len(present)-1] + "\n"))
		}
		for _, b := range band {
			buf.Write(b)
		}
		buf.Write(pktFlush)
	}
	return bodyReply(200, "application/x-git-upload-pack-result", buf.Bytes())
}

func splitSections(packets []Packet) [][]string {
	sections := [][]string{{}}
	for _, packet := range packets {
		if packet.Kind == "data" {
			line := strings.TrimSuffix(string(packet.Data), "\n")
			sections[len(sections)-1] = append(sections[len(sections)-1], line)
		} else if packet.Kind == "delim" || packet.Kind == "flush" {
			sections = append(sections, []string{})
		}
	}
	return sections
}

func handleUploadPack(ctx *Ctx, repo *Repo) Reply {
	isV2 := strings.Contains(ctx.header("git-protocol"), "version=2")
	packets, _ := decodePackets(ctx.RawBody, 0)
	sections := splitSections(packets)
	if isV2 {
		head := sectionAt(sections, 0)
		command := ""
		for _, l := range head {
			if strings.HasPrefix(l, "command=") {
				command = l
				break
			}
		}
		if command == "" {
			return bodyReply(400, "text/plain", []byte("invalid upload-pack request"))
		}
		args := sectionAt(sections, 1)
		if command == "command=ls-refs" {
			return lsRefs(repo, args)
		}
		if command == "command=fetch" {
			var wants, haves, shallow []string
			var depth *int
			done := false
			deepenRelative := false
			for _, line := range args {
				switch {
				case strings.HasPrefix(line, "want "):
					wants = append(wants, strings.TrimSpace(line[5:]))
				case strings.HasPrefix(line, "have "):
					haves = append(haves, strings.TrimSpace(line[5:]))
				case strings.HasPrefix(line, "shallow "):
					shallow = append(shallow, strings.TrimSpace(line[8:]))
				case strings.HasPrefix(line, "deepen "):
					n, _ := strconv.Atoi(strings.TrimSpace(line[7:]))
					depth = &n
				case line == "deepen-relative":
					deepenRelative = true
				case line == "done":
					done = true
				}
			}
			return fetchReply(repo, wants, haves, shallow, depth, true, done, deepenRelative)
		}
		return bodyReply(400, "text/plain", []byte("unknown command"))
	}
	// v0 fetch
	wantSection := sectionAt(sections, 0)
	haveSection := sectionAt(sections, 1)
	var wants, shallow []string
	var depth *int
	for _, line := range wantSection {
		switch {
		case strings.HasPrefix(line, "want "):
			oid := strings.Fields(strings.TrimSpace(line[5:]))
			if len(oid) > 0 {
				wants = append(wants, oid[0])
			}
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, _ := strconv.Atoi(strings.TrimSpace(line[7:]))
			depth = &n
		}
	}
	var haves []string
	for _, line := range haveSection {
		if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		}
	}
	if len(wants) == 0 {
		return bodyReply(400, "text/plain", []byte("invalid upload-pack request"))
	}
	return fetchReply(repo, wants, haves, shallow, depth, false, true, false)
}

func sectionAt(sections [][]string, i int) []string {
	if i < len(sections) {
		return sections[i]
	}
	return nil
}

// -------------------------------------------------------------- receive-pack
type refCommand struct {
	oldOID string
	newOID string
	ref    string
}

var refCommandRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (.+)$`)

func handleReceivePack(ctx *Ctx, repo *Repo) Reply {
	packets, rest := decodePackets(ctx.RawBody, 0)
	var commands []refCommand
	capabilities := ""
	sawFlush := false
	for _, packet := range packets {
		if packet.Kind == "flush" {
			sawFlush = true
			break
		}
		if packet.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(packet.Data), "\n")
		if nul := strings.IndexByte(line, 0); nul != -1 {
			capabilities = line[nul+1:]
			line = line[:nul]
		}
		m := refCommandRe.FindStringSubmatch(line)
		if m == nil {
			return bodyReply(400, "text/plain", []byte("invalid receive-pack request"))
		}
		commands = append(commands, refCommand{oldOID: m[1], newOID: m[2], ref: m[3]})
	}
	if !sawFlush && len(commands) == 0 {
		return bodyReply(400, "text/plain", []byte("invalid receive-pack request"))
	}

	unpackLine := "unpack ok"
	if len(rest) > 0 {
		objects, err := readPack(rest, func(oid string) *GitObject { return repo.Objects[oid] })
		if err != nil {
			unpackLine = "unpack " + err.Error()
		} else {
			for oid, obj := range objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = obj
				}
			}
		}
	}

	type result struct {
		ref    string
		ok     bool
		reason string
	}
	var results []result
	if !strings.HasPrefix(unpackLine, "unpack ok") {
		for _, command := range commands {
			results = append(results, result{ref: command.ref, reason: "unpack failed"})
		}
	} else {
		for _, command := range commands {
			current, hasCurrent := repo.Refs[command.ref]
			if !hasCurrent {
				current = ZeroOID
			}
			if command.newOID == ZeroOID {
				if !hasCurrent {
					results = append(results, result{ref: command.ref, reason: "reference not found"})
				} else if command.oldOID != current {
					results = append(results, result{ref: command.ref, reason: "stale info"})
				} else {
					repo.deleteRef(command.ref)
					repo.Version++
					results = append(results, result{ref: command.ref, ok: true})
				}
				continue
			}
			if _, ok := repo.Objects[command.newOID]; !ok {
				results = append(results, result{ref: command.ref, reason: "missing object"})
				continue
			}
			if command.oldOID != ZeroOID && command.oldOID != current {
				results = append(results, result{ref: command.ref, reason: "stale info"})
				continue
			}
			if command.oldOID != ZeroOID && current == command.oldOID && command.oldOID != command.newOID {
				if !isAncestor(repo, command.oldOID, command.newOID) {
					results = append(results, result{ref: command.ref, reason: "non-fast-forward"})
					continue
				}
			}
			repo.setRef(command.ref, command.newOID)
			repo.Version++
			results = append(results, result{ref: command.ref, ok: true})
		}
	}

	var report bytes.Buffer
	report.Write(pktLineStr(unpackLine + "\n"))
	for _, r := range results {
		if r.ok {
			report.Write(pktLineStr("ok " + r.ref + "\n"))
		} else {
			report.Write(pktLineStr("ng " + r.ref + " " + r.reason + "\n"))
		}
	}
	report.Write(pktFlush)
	reportBytes := report.Bytes()

	var body []byte
	if strings.Contains(capabilities, "side-band-64k") {
		var buf bytes.Buffer
		const chunk = 32768
		for offset := 0; offset < len(reportBytes); offset += chunk {
			end := offset + chunk
			if end > len(reportBytes) {
				end = len(reportBytes)
			}
			msg := make([]byte, 0, end-offset+1)
			msg = append(msg, 1)
			msg = append(msg, reportBytes[offset:end]...)
			buf.Write(pktLine(msg))
		}
		buf.Write(pktFlush)
		body = buf.Bytes()
	} else {
		body = reportBytes
	}
	return bodyReply(200, "application/x-git-receive-pack-result", body)
}

// ------------------------------------------------------------------- router
func handleGit(ctx *Ctx) Reply {
	store := ctx.Store
	owner := ctx.Segments[0]
	repoGit := ctx.Segments[1]
	name := strings.TrimSuffix(repoGit, ".git")
	store.learnViewer(owner)
	repo := store.resolveRepo(owner, name)
	if repo == nil {
		return errorReply(404, "Not Found")
	}
	rest := ctx.Segments[2:]

	if len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" && ctx.Method == "GET" {
		service := ""
		if vals, ok := ctx.Query["service"]; ok && len(vals) > 0 {
			service = vals[0]
		}
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return bodyReply(403, "text/plain", []byte("unsupported service"))
		}
		return advertisement(ctx, repo, service)
	}
	if len(rest) == 1 && rest[0] == "git-upload-pack" && ctx.Method == "POST" {
		return handleUploadPack(ctx, repo)
	}
	if len(rest) == 1 && rest[0] == "git-receive-pack" && ctx.Method == "POST" {
		return handleReceivePack(ctx, repo)
	}
	return errorReply(404, "Not Found")
}
