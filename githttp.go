package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type cachedPackKey struct {
	repoName   string
	ver        int
	wants      string
	haves      string
	boundaries string
	depth      int
	hasDepth   bool
}

type cachedAdKey struct {
	repoName string
	ver      int
	service  string
	v2       bool
}

var (
	fetchPlanCache = map[cachedPackKey]fetchPlan{}
	adCache        = map[cachedAdKey][]byte{}
)

func peelToCommit(repo *Repo, sha string) string {
	cur := sha
	for i := 0; i < 10; i++ {
		obj, ok := repo.Objects[cur]
		if !ok {
			return cur
		}
		if obj.Type == ObjCommit {
			return cur
		}
		if obj.Type == ObjTag {
			text := string(obj.Data)
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "object ") {
					cur = strings.TrimSpace(line[7:])
					break
				}
			}
			continue
		}
		return cur
	}
	return cur
}

func sortedRefEntries(repo *Repo) [][2]string {
	var entries [][2]string
	for name, oid := range repo.Refs {
		entries = append(entries, [2]string{name, oid})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i][0] < entries[j][0]
	})
	return entries
}

func objectClosure(repo *Repo, roots []string) map[string]bool {
	out := make(map[string]bool)
	stack := make([]string, len(roots))
	copy(stack, roots)
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[sha] {
			continue
		}
		obj, ok := repo.Objects[sha]
		if !ok {
			continue
		}
		out[sha] = true
		if obj.Type == ObjCommit {
			c := parseCommit(obj.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		} else if obj.Type == ObjTree {
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.Sha)
			}
		} else if obj.Type == ObjTag {
			text := string(obj.Data)
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "object ") {
					stack = append(stack, strings.TrimSpace(line[7:]))
					break
				}
			}
		}
	}
	return out
}

func advertisement(repo *Repo, service string, v2 bool) []byte {
	cachedKey := cachedAdKey{repoName: repo.Name, ver: repo.Ver, service: service, v2: v2}
	Mu.RLock()
	if cached, ok := adCache[cachedKey]; ok {
		Mu.RUnlock()
		return cached
	}
	Mu.RUnlock()

	Mu.Lock()
	defer Mu.Unlock()
	if cached, ok := adCache[cachedKey]; ok {
		return cached
	}

	var res []byte
	if service == "git-upload-pack" && v2 {
		lines := []string{"version 2\n", "agent=forge/1.0\n", "object-format=sha1\n", "ls-refs=unborn\n", "fetch=shallow wait-for-done\n"}
		var parts []byte
		for _, l := range lines {
			parts = append(parts, pktLineStr(l)...)
		}
		parts = append(parts, PktFlush...)
		res = parts
		adCache[cachedKey] = res
		return res
	}

	caps := "multi_ack_detailed no-done side-band-64k ofs-delta shallow symref=HEAD:refs/heads/" + repo.DefaultBranch + " agent=forge/1.0"
	if service == "git-receive-pack" {
		caps = "report-status delete-refs side-band-64k ofs-delta agent=forge/1.0"
	}

	var parts [][]byte
	parts = append(parts, pktLineStr("# service="+service+"\n"))
	parts = append(parts, PktFlush)

	refs := sortedRefEntries(repo)
	headRef := "refs/heads/" + repo.DefaultBranch
	headTip := repo.Refs[headRef]

	if len(refs) == 0 {
		parts = append(parts, pktLine([]byte(ZeroSha+" capabilities^{}\x00"+caps+"\n")))
	} else {
		first := true
		emit := func(oid, name string) {
			if first {
				parts = append(parts, pktLine([]byte(oid+" "+name+"\x00"+caps+"\n")))
				first = false
			} else {
				parts = append(parts, pktLine([]byte(oid+" "+name+"\n")))
			}
		}
		if headTip != "" {
			emit(headTip, "HEAD")
		}
		for _, ref := range refs {
			name, oid := ref[0], ref[1]
			emit(oid, name)
			obj := repo.Objects[oid]
			if obj.Type == ObjTag {
				emit(peelToCommit(repo, oid), name+"^{}")
			}
		}
		if headTip == "" && first {
			parts = append(parts, pktLine([]byte(ZeroSha+" capabilities^{}\x00"+caps+"\n")))
		}
	}
	parts = append(parts, PktFlush)
	res = bytes.Join(parts, nil)
	adCache[cachedKey] = res
	return res
}

func lsRefs(repo *Repo, body []byte) []byte {
	packets, _ := decodePkts(body, 0)
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	pastDelim := false
	for _, p := range packets {
		if p.Kind == 2 { // delim
			pastDelim = true
			continue
		}
		if p.Kind == 1 { // flush
			break
		}
		if p.Kind != 0 || !pastDelim {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		if line == "peel" {
			peel = true
		} else if line == "symrefs" {
			symrefs = true
		} else if line == "unborn" {
			unborn = true
		} else if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}

	keep := func(name string) bool {
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

	var out [][]byte
	headRef := "refs/heads/" + repo.DefaultBranch
	headTip := repo.Refs[headRef]
	if keep("HEAD") {
		if headTip != "" {
			symStr := ""
			if symrefs {
				symStr = " symref-target:" + headRef
			}
			out = append(out, pktLine([]byte(headTip+" HEAD"+symStr+"\n")))
		} else if unborn {
			symStr := ""
			if symrefs {
				symStr = " symref-target:" + headRef
			}
			out = append(out, pktLine([]byte("unborn HEAD"+symStr+"\n")))
		}
	}

	for _, ref := range sortedRefEntries(repo) {
		name, oid := ref[0], ref[1]
		if !keep(name) {
			continue
		}
		line := oid + " " + name
		if peel {
			obj := repo.Objects[oid]
			if obj.Type == ObjTag {
				line += " peeled:" + peelToCommit(repo, oid)
			}
		}
		out = append(out, pktLine([]byte(line+"\n")))
	}
	out = append(out, PktFlush)
	return bytes.Join(out, nil)
}

func closureCutAt(repo *Repo, roots []string, cut map[string]bool) map[string]bool {
	out := make(map[string]bool)
	stack := make([]string, len(roots))
	copy(stack, roots)
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[sha] {
			continue
		}
		obj, ok := repo.Objects[sha]
		if !ok {
			continue
		}
		out[sha] = true
		if obj.Type == ObjCommit {
			c := parseCommit(obj.Data)
			stack = append(stack, c.Tree)
			if !cut[sha] {
				stack = append(stack, c.Parents...)
			}
		} else if obj.Type == ObjTree {
			for _, e := range parseTree(obj.Data) {
				stack = append(stack, e.Sha)
			}
		} else if obj.Type == ObjTag {
			text := string(obj.Data)
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "object ") {
					stack = append(stack, strings.TrimSpace(line[7:]))
					break
				}
			}
		}
	}
	return out
}

type fetchPlan struct {
	pack       []byte
	shallow    []string
	unshallow  []string
	commonHave string
	hasCommon  bool
}

func planFetch(repo *Repo, wants, haves, boundaries []string, depth int, hasDepth bool) fetchPlan {
	boundarySetPre := make(map[string]bool)
	for _, b := range boundaries {
		if _, ok := repo.Objects[b]; ok {
			boundarySetPre[b] = true
		}
	}
	var validHaves []string
	for _, h := range haves {
		if _, ok := repo.Objects[h]; ok {
			validHaves = append(validHaves, h)
		}
	}
	hadClosure := closureCutAt(repo, validHaves, boundarySetPre)
	boundarySet := boundarySetPre
	hadObjects := make(map[string]bool)
	for k := range hadClosure {
		hadObjects[k] = true
	}
	for b := range boundarySet {
		hadObjects[b] = true
		obj := repo.Objects[b]
		if obj.Type == ObjCommit {
			c := parseCommit(obj.Data)
			for sha := range objectClosure(repo, []string{c.Tree}) {
				hadObjects[sha] = true
			}
		}
	}

	included := make(map[string]int)
	type queueItem struct {
		sha string
		gen int
	}
	var queue []queueItem
	var tagObjects []string

	for _, w := range wants {
		obj, ok := repo.Objects[w]
		if !ok {
			continue
		}
		if obj.Type == ObjTag {
			tagObjects = append(tagObjects, w)
			target := peelToCommit(repo, w)
			queue = append(queue, queueItem{sha: target, gen: 0})
		} else if obj.Type == ObjCommit {
			queue = append(queue, queueItem{sha: w, gen: 0})
		}
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if gen, ok := included[item.sha]; ok && gen <= item.gen {
			continue
		}
		if item.gen > 0 && hadClosure[item.sha] {
			continue
		}
		if hasDepth && item.gen >= depth {
			continue
		}
		included[item.sha] = item.gen
		obj, ok := repo.Objects[item.sha]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		c := parseCommit(obj.Data)
		for _, parent := range c.Parents {
			queue = append(queue, queueItem{sha: parent, gen: item.gen + 1})
		}
	}

	sendMap := make(map[string]GitObj)
	addObj := func(sha string) {
		if hadObjects[sha] {
			return
		}
		if _, ok := sendMap[sha]; ok {
			return
		}
		if obj, ok := repo.Objects[sha]; ok {
			sendMap[sha] = obj
		}
	}

	for _, t := range tagObjects {
		addObj(t)
	}

	var shallow []string
	var unshallow []string

	for sha := range included {
		obj, ok := repo.Objects[sha]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		c := parseCommit(obj.Data)
		if !boundarySet[sha] {
			addObj(sha)
			for o := range objectClosure(repo, []string{c.Tree}) {
				addObj(o)
			}
		}
		var parentsInRepo []string
		var parentsIncluded []string
		for _, p := range c.Parents {
			if _, ok := repo.Objects[p]; ok {
				parentsInRepo = append(parentsInRepo, p)
				if _, ok := included[p]; ok {
					parentsIncluded = append(parentsIncluded, p)
				}
			}
		}
		if len(parentsInRepo) > 0 && len(parentsIncluded) < len(parentsInRepo) {
			shallow = append(shallow, sha)
		} else if boundarySet[sha] {
			unshallow = append(unshallow, sha)
		}
	}

	var objsList []GitObj
	for _, o := range sendMap {
		objsList = append(objsList, o)
	}
	pack := buildPack(objsList)

	var commonHave string
	for _, h := range haves {
		if _, ok := repo.Objects[h]; ok {
			commonHave = h
			break
		}
	}

	return fetchPlan{
		pack:       pack,
		shallow:    shallow,
		unshallow:  unshallow,
		commonHave: commonHave,
		hasCommon:  commonHave != "",
	}
}

func getPlanCached(repo *Repo, wants, haves, boundaries []string, depth int, hasDepth bool) fetchPlan {
	sortedWants := make([]string, len(wants))
	copy(sortedWants, wants)
	sort.Strings(sortedWants)
	wantsKey := strings.Join(sortedWants, ",")
	sortedHaves := make([]string, len(haves))
	copy(sortedHaves, haves)
	sort.Strings(sortedHaves)
	havesKey := strings.Join(sortedHaves, ",")
	sortedBoundaries := make([]string, len(boundaries))
	copy(sortedBoundaries, boundaries)
	sort.Strings(sortedBoundaries)
	boundariesKey := strings.Join(sortedBoundaries, ",")
	cacheKey := cachedPackKey{
		repoName:   repo.Name,
		ver:        repo.Ver,
		wants:      wantsKey,
		haves:      havesKey,
		boundaries: boundariesKey,
		depth:      depth,
		hasDepth:   hasDepth,
	}

	Mu.RLock()
	if p, ok := fetchPlanCache[cacheKey]; ok {
		Mu.RUnlock()
		return p
	}
	Mu.RUnlock()

	Mu.Lock()
	defer Mu.Unlock()
	if p, ok := fetchPlanCache[cacheKey]; ok {
		return p
	}
	p := planFetch(repo, wants, haves, boundaries, depth, hasDepth)
	fetchPlanCache[cacheKey] = p
	return p
}

func sidebandChunks(pack []byte) [][]byte {
	var out [][]byte
	const CHUNK = 65515
	for i := 0; i < len(pack); i += CHUNK {
		end := i + CHUNK
		if end > len(pack) {
			end = len(pack)
		}
		chunkData := append([]byte{1}, pack[i:end]...)
		out = append(out, pktLine(chunkData))
	}
	if len(pack) == 0 {
		out = append(out, pktLine([]byte{1}))
	}
	return out
}

func fetchV2(repo *Repo, body []byte) []byte {
	packets, _ := decodePkts(body, 0)
	var wants, haves, boundaries []string
	depth := 0
	hasDepth := false
	for _, p := range packets {
		if p.Kind != 0 {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		if strings.HasPrefix(line, "want ") {
			wants = append(wants, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			boundaries = append(boundaries, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			d, err := strconv.Atoi(strings.TrimSpace(line[7:]))
			if err == nil {
				depth = d
				hasDepth = true
			}
		}
	}
	plan := getPlanCached(repo, wants, haves, boundaries, depth, hasDepth)
	var out [][]byte
	if len(haves) > 0 {
		out = append(out, pktLine([]byte("acknowledgments\n")))
		if plan.hasCommon {
			out = append(out, pktLine([]byte("ACK "+plan.commonHave+"\n")))
			out = append(out, pktLine([]byte("ready\n")))
		} else {
			out = append(out, pktLine([]byte("NAK\n")))
		}
		out = append(out, PktDelim)
	}
	if hasDepth || len(boundaries) > 0 {
		out = append(out, pktLine([]byte("shallow-info\n")))
		for _, s := range plan.shallow {
			out = append(out, pktLine([]byte("shallow "+s+"\n")))
		}
		for _, u := range plan.unshallow {
			out = append(out, pktLine([]byte("unshallow "+u+"\n")))
		}
		out = append(out, PktDelim)
	}
	out = append(out, pktLine([]byte("packfile\n")))
	for _, chunk := range sidebandChunks(plan.pack) {
		out = append(out, chunk)
	}
	out = append(out, PktFlush)
	return bytes.Join(out, nil)
}

func fetchV0(repo *Repo, body []byte) []byte {
	packets, _ := decodePkts(body, 0)
	var wants, haves, boundaries []string
	depth := 0
	hasDepth := false
	for _, p := range packets {
		if p.Kind != 0 {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		if idx := strings.IndexByte(line, 0); idx >= 0 {
			line = line[:idx]
		}
		if strings.HasPrefix(line, "want ") {
			w := strings.TrimSpace(line[5:])
			if sp := strings.IndexByte(w, ' '); sp >= 0 {
				w = w[:sp]
			}
			wants = append(wants, w)
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			boundaries = append(boundaries, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			d, err := strconv.Atoi(strings.TrimSpace(line[7:]))
			if err == nil {
				depth = d
				hasDepth = true
			}
		}
	}
	plan := getPlanCached(repo, wants, haves, boundaries, depth, hasDepth)
	var out [][]byte
	for _, s := range plan.shallow {
		out = append(out, pktLine([]byte("shallow "+s+"\n")))
	}
	for _, u := range plan.unshallow {
		out = append(out, pktLine([]byte("unshallow "+u+"\n")))
	}
	if plan.hasCommon {
		out = append(out, pktLine([]byte("ACK "+plan.commonHave+"\n")))
	} else {
		out = append(out, pktLine([]byte("NAK\n")))
	}
	for _, chunk := range sidebandChunks(plan.pack) {
		out = append(out, chunk)
	}
	out = append(out, PktFlush)
	return bytes.Join(out, nil)
}

func receivePack(repo *Repo, body []byte) []byte {
	packets, rest := decodePkts(body, 0)
	type Cmd struct {
		oldId string
		newId string
		ref   string
	}
	var cmds []Cmd
	for _, p := range packets {
		if p.Kind == 1 { // flush
			break
		}
		if p.Kind != 0 {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		if idx := strings.IndexByte(line, 0); idx >= 0 {
			line = line[:idx]
		}
		parts := strings.Split(line, " ")
		if len(parts) >= 3 {
			cmds = append(cmds, Cmd{oldId: parts[0], newId: parts[1], ref: parts[2]})
		}
	}

	packBuf := body[rest:]
	unpackOk := true
	newObjects := make(map[string]GitObj)
	if len(packBuf) > 0 {
		parsed, err := parsePack(packBuf, func(sha string) *GitObj {
			if obj, ok := repo.Objects[sha]; ok {
				return &obj
			}
			return nil
		})
		if err != nil {
			unpackOk = false
		} else {
			newObjects = parsed.Objects
		}
	}

	lookup := func(sha string) *GitObj {
		if obj, ok := newObjects[sha]; ok {
			return &obj
		}
		if obj, ok := repo.Objects[sha]; ok {
			return &obj
		}
		return nil
	}

	descends := func(from, to string) bool {
		seen := make(map[string]bool)
		stack := []string{to}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if cur == from {
				return true
			}
			if seen[cur] {
				continue
			}
			seen[cur] = true
			obj := lookup(cur)
			if obj != nil && obj.Type == ObjCommit {
				c := parseCommit(obj.Data)
				stack = append(stack, c.Parents...)
			}
		}
		return false
	}

	type Result struct {
		ref    string
		ok     bool
		reason string
	}
	var results []Result

	if unpackOk {
		for sha, obj := range newObjects {
			if _, ok := repo.Objects[sha]; !ok {
				repo.Objects[sha] = obj
				repo.Ver++
			}
		}
		for _, cmd := range cmds {
			current := repo.Refs[cmd.ref]
			if cmd.newId == ZeroSha {
				if current != "" {
					delete(repo.Refs, cmd.ref)
					repo.Ver++
					results = append(results, Result{ref: cmd.ref, ok: true})
				} else {
					results = append(results, Result{ref: cmd.ref, ok: false, reason: "reference does not exist"})
				}
				continue
			}
			if lookup(cmd.newId) == nil {
				results = append(results, Result{ref: cmd.ref, ok: false, reason: "missing object"})
				continue
			}
			if cmd.oldId == ZeroSha {
				repo.Refs[cmd.ref] = cmd.newId
				repo.Ver++
				results = append(results, Result{ref: cmd.ref, ok: true})
				continue
			}
			if current != cmd.oldId {
				results = append(results, Result{ref: cmd.ref, ok: false, reason: "stale ref"})
				continue
			}
			newObj := lookup(cmd.newId)
			if newObj != nil && newObj.Type == ObjCommit && !descends(cmd.oldId, cmd.newId) {
				results = append(results, Result{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
			repo.Refs[cmd.ref] = cmd.newId
			repo.Ver++
			results = append(results, Result{ref: cmd.ref, ok: true})
		}
	}

	var reportLines [][]byte
	if unpackOk {
		reportLines = append(reportLines, pktLine([]byte("unpack ok\n")))
		for _, r := range results {
			if r.ok {
				reportLines = append(reportLines, pktLine([]byte("ok "+r.ref+"\n")))
			} else {
				reportLines = append(reportLines, pktLine([]byte("ng "+r.ref+" "+r.reason+"\n")))
			}
		}
	} else {
		reportLines = append(reportLines, pktLine([]byte("unpack error\n")))
	}
	reportLines = append(reportLines, PktFlush)
	report := bytes.Join(reportLines, nil)

	var out [][]byte
	const CHUNK = 65515
	for i := 0; i < len(report); i += CHUNK {
		end := i + CHUNK
		if end > len(report) {
			end = len(report)
		}
		chunkData := append([]byte{1}, report[i:end]...)
		out = append(out, pktLine(chunkData))
	}
	out = append(out, PktFlush)
	return bytes.Join(out, nil)
}

// lookupRepo resolves a repo without locking; callers must already hold Mu
// (routeApi and handleGraphQL hold Mu.Lock for the whole request, and a
// nested RLock there deadlocks). findRepo is the locking wrapper for
// callers that do not hold Mu (the git HTTP routes).
func lookupRepo(owner, name string) *Repo {
	if owner != ViewerLogin {
		return nil
	}
	return repos[name]
}

func findRepo(owner, name string) *Repo {
	Mu.RLock()
	defer Mu.RUnlock()
	return lookupRepo(owner, name)
}

func routeGit(w http.ResponseWriter, r *http.Request, raw []byte) bool {
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		return false
	}
	owner := parts[1]
	repoPart := parts[2]
	if !strings.HasSuffix(repoPart, ".git") {
		return false
	}
	repoName := strings.TrimSuffix(repoPart, ".git")
	action := strings.Join(parts[3:], "/")

	validAction := action == "info/refs" || action == "git-upload-pack" || action == "git-receive-pack"
	if !validAction {
		return false
	}

	if !authed(r) {
		sendJson(w, 401, map[string]string{"message": "Bad credentials"}, nil)
		return true
	}

	repo := findRepo(owner, repoName)
	if repo == nil {
		sendJson(w, 404, map[string]string{"message": "Not Found"}, nil)
		return true
	}

	service := r.URL.Query().Get("service")

	if r.Method == "GET" && action == "info/refs" {
		if service != "git-upload-pack" && service != "git-receive-pack" {
			sendJson(w, 404, map[string]string{"message": "Not Found"}, nil)
			return true
		}
		v2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
		advBuf := advertisement(repo, service, v2)
		contentType := "application/x-" + service + "-advertisement"
		sendBuffer(w, 200, advBuf, contentType)
		return true
	}

	if r.Method == "POST" && action == "git-upload-pack" {
		packets, _ := decodePkts(raw, 0)
		firstText := ""
		for _, p := range packets {
			if p.Kind == 0 {
				firstText = string(p.Payload)
				break
			}
		}
		var buf []byte
		if strings.HasPrefix(firstText, "command=ls-refs") {
			buf = lsRefs(repo, raw)
		} else if strings.HasPrefix(firstText, "command=fetch") {
			buf = fetchV2(repo, raw)
		} else {
			buf = fetchV0(repo, raw)
		}
		sendBuffer(w, 200, buf, "application/x-git-upload-pack-result")
		return true
	}

	if r.Method == "POST" && action == "git-receive-pack" {
		Mu.Lock()
		buf := receivePack(repo, raw)
		Mu.Unlock()
		sendBuffer(w, 200, buf, "application/x-git-receive-pack-result")
		return true
	}

	sendJson(w, 404, map[string]string{"message": "Not Found"}, nil)
	return true
}

func sendBuffer(w http.ResponseWriter, status int, buf []byte, contentType string) {
	requestCounterAtomic++
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.Header().Set("X-GitHub-Request-ID", fmt.Sprintf("req-%08x", requestCounterAtomic))
	w.WriteHeader(status)
	w.Write(buf)
}
