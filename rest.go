package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func repoJson(repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      ViewerLogin + "/" + repo.Name,
		"owner":          map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"private":        repo.IsPrivate,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     isoOf(EpochSec + int64(repo.CreatedSeq)),
		"updated_at":     isoOf(EpochSec + int64(repo.CreatedSeq)),
		"html_url":       "http://forge/" + ViewerLogin + "/" + repo.Name,
	}
}

func issueJson(repo *Repo, rec *IssueRec) map[string]any {
	labels := make([]map[string]any, len(rec.Labels))
	for i, name := range rec.Labels {
		color := "ededed"
		if lbl, ok := repo.Labels[name]; ok {
			color = lbl.Color
		}
		labels[i] = map[string]any{"name": name, "color": color}
	}
	base := map[string]any{
		"id":         rec.ID,
		"number":     rec.Number,
		"title":      rec.Title,
		"body":       rec.Body,
		"state":      rec.State,
		"labels":     labels,
		"user":       map[string]any{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"comments":   len(rec.Comments),
		"created_at": isoOf(EpochSec + int64(rec.CreatedSeq)),
		"updated_at": isoOf(EpochSec + int64(rec.CreatedSeq)),
	}
	if rec.Kind == "pull" {
		base["pull_request"] = map[string]any{
			"url": "/api/v3/repos/" + ViewerLogin + "/" + repo.Name + "/pulls/" + strconv.Itoa(rec.Number),
		}
	}
	return base
}

func pullJson(repo *Repo, rec *IssueRec) map[string]any {
	var headShaStr, baseShaStr string
	if rec.HeadRef != "" {
		if s := branchTip(repo, rec.HeadRef); s != "" {
			headShaStr = s
		}
	}
	if rec.BaseRef != "" {
		if s := branchTip(repo, rec.BaseRef); s != "" {
			baseShaStr = s
		}
	}
	var mergeable any = nil
	if rec.State == "open" && headShaStr != "" && baseShaStr != "" {
		_, ok := mergeTrees(repo, baseShaStr, headShaStr)
		mergeable = ok
	}
	ij := issueJson(repo, rec)
	state := ij["state"]
	if rec.Merged {
		state = "closed"
	}
	ij["state"] = state

	var mergedAt any = nil
	if rec.Merged {
		ts := rec.MergedTs
		if ts == 0 {
			ts = EpochSec
		}
		mergedAt = isoOf(ts)
	}
	var mergeCommitSha any = nil
	if rec.HasMergeSha {
		mergeCommitSha = rec.MergeCommitSha
	}
	var headShaVal any = nil
	if headShaStr != "" {
		headShaVal = headShaStr
	}
	var baseShaVal any = nil
	if baseShaStr != "" {
		baseShaVal = baseShaStr
	}

	ij["head"] = map[string]any{
		"ref":   rec.HeadRef,
		"sha":   headShaVal,
		"label": ViewerLogin + ":" + rec.HeadRef,
	}
	ij["base"] = map[string]any{
		"ref":   rec.BaseRef,
		"sha":   baseShaVal,
		"label": ViewerLogin + ":" + rec.BaseRef,
	}
	ij["merged"] = rec.Merged
	ij["merged_at"] = mergedAt
	ij["merge_commit_sha"] = mergeCommitSha
	ij["mergeable"] = mergeable
	ij["draft"] = false
	return ij
}

func pathUnescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

func parseJsonBody(raw []byte) (map[string]any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok || obj == nil {
		return nil, false
	}
	return obj, true
}

func paginate[T any](u *url.URL, items []T, host string) ([]T, string) {
	q := u.Query()
	perPage := 30
	if pp := q.Get("per_page"); pp != "" {
		if n, err := strconv.Atoi(pp); err == nil {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}

	pageNum := 1
	if pn := q.Get("page"); pn != "" {
		if n, err := strconv.Atoi(pn); err == nil {
			pageNum = n
		}
	}
	if pageNum < 1 {
		pageNum = 1
	}

	start := (pageNum - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]

	var link string
	if start+perPage < len(items) {
		mk := func(p int) string {
			nu := *u
			nq := nu.Query()
			nq.Set("page", strconv.Itoa(p))
			nq.Set("per_page", strconv.Itoa(perPage))
			nu.RawQuery = nq.Encode()
			return "http://" + host + nu.Path + "?" + nu.RawQuery
		}
		lastPage := (len(items) + perPage - 1) / perPage
		link = "<" + mk(pageNum+1) + ">; rel=\"next\", <" + mk(lastPage) + ">; rel=\"last\""
	}
	return slice, link
}

var identRegex = regexp.MustCompile(`^(.*) <(.*)> (\d+) [+-]\d{4}$`)

func identParts(line string) map[string]any {
	m := identRegex.FindStringSubmatch(line)
	if m == nil {
		return map[string]any{"name": line, "email": "", "date": isoOf(EpochSec)}
	}
	ts, _ := strconv.ParseInt(m[3], 10, 64)
	return map[string]any{"name": m[1], "email": m[2], "date": isoOf(ts)}
}

func commitJson(repo *Repo, sha string) map[string]any {
	tree := ""
	var parents []string
	message := ""
	authorLine := ""
	committerLine := ""
	if obj, ok := repo.Objects[sha]; ok && obj.Type == "commit" {
		info := parseCommit(obj.Data)
		tree = info.Tree
		parents = info.Parents
		message = info.Message
		authorLine = info.AuthorLine
		committerLine = info.CommitterLine
	}
	parentsArr := make([]map[string]any, len(parents))
	for i, p := range parents {
		parentsArr[i] = map[string]any{"sha": p}
	}
	msgTrimmed := strings.TrimRight(message, "\n")
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"tree":      map[string]any{"sha": tree},
			"message":   msgTrimmed,
			"author":    identParts(authorLine),
			"committer": identParts(committerLine),
		},
		"parents":   parentsArr,
		"author":    map[string]any{"login": ViewerLogin, "id": ViewerID},
		"committer": map[string]any{"login": ViewerLogin, "id": ViewerID},
	}
}

var committerTsRegex = regexp.MustCompile(`(\d+) [+-]\d{4}`)

type commitTs struct {
	sha string
	ts  int64
}

func listCommits(repo *Repo, fromSha string) []string {
	seen := map[string]bool{}
	stack := []string{fromSha}
	var found []commitTs

	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true

		obj, ok := repo.Objects[cur]
		if !ok || obj.Type != "commit" {
			continue
		}
		info := parseCommit(obj.Data)
		var ts int64
		if m := committerTsRegex.FindStringSubmatch(info.CommitterLine); m != nil {
			ts, _ = strconv.ParseInt(m[1], 10, 64)
		}
		found = append(found, commitTs{sha: cur, ts: ts})
		for _, p := range info.Parents {
			stack = append(stack, p)
		}
	}

	sort.Slice(found, func(i, j int) bool {
		return found[i].ts > found[j].ts
	})

	res := make([]string, len(found))
	for i, f := range found {
		res[i] = f.sha
	}
	return res
}

type diffResult struct {
	Additions int
	Deletions int
}

func lineDiff(a, b string) diffResult {
	var al, bl []string
	if a != "" {
		al = strings.Split(a, "\n")
	}
	if b != "" {
		bl = strings.Split(b, "\n")
	}
	dp := make([]int, len(bl)+1)
	for i := 1; i <= len(al); i++ {
		prev := 0
		for j := 1; j <= len(bl); j++ {
			tmp := dp[j]
			if al[i-1] == bl[j-1] {
				dp[j] = prev + 1
			} else {
				if dp[j] < dp[j-1] {
					dp[j] = dp[j-1]
				}
			}
			prev = tmp
		}
	}
	common := dp[len(bl)]
	return diffResult{
		Additions: len(bl) - common,
		Deletions: len(al) - common,
	}
}

func pullFiles(repo *Repo, rec *IssueRec) []map[string]any {
	var headSha, baseSha string
	if rec.HeadRef != "" {
		headSha = branchTip(repo, rec.HeadRef)
	}
	if rec.BaseRef != "" {
		baseSha = branchTip(repo, rec.BaseRef)
	}
	if headSha == "" || baseSha == "" {
		return []map[string]any{}
	}
	mb, ok := mergeBase(repo, baseSha, headSha)
	if !ok {
		mb = ""
	}
	fromFiles := flattenTree(repo, treeOfCommit(repo, mb), "")
	toFiles := flattenTree(repo, treeOfCommit(repo, headSha), "")

	pathSet := map[string]bool{}
	for k := range fromFiles {
		pathSet[k] = true
	}
	for k := range toFiles {
		pathSet[k] = true
	}
	var paths []string
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []map[string]any
	for _, p := range paths {
		from := fromFiles[p]
		to := toFiles[p]
		if from == to {
			continue
		}
		fromText := ""
		if from != "" {
			if obj, ok := repo.Objects[from]; ok {
				fromText = string(obj.Data)
			}
		}
		toText := ""
		if to != "" {
			if obj, ok := repo.Objects[to]; ok {
				toText = string(obj.Data)
			}
		}
		diff := lineDiff(fromText, toText)
		status := "modified"
		if from == "" {
			status = "added"
		} else if to == "" {
			status = "removed"
		}
		var shaVal any = to
		if shaVal == "" {
			shaVal = from
		}
		out = append(out, map[string]any{
			"filename":  p,
			"status":    status,
			"additions": diff.Additions,
			"deletions": diff.Deletions,
			"changes":   diff.Additions + diff.Deletions,
			"sha":       shaVal,
		})
	}
	return out
}

func routeApi(w http.ResponseWriter, r *http.Request, raw []byte) {
	Mu.Lock()
	defer Mu.Unlock()

	url := r.URL
	path := url.Path
	segments := strings.Split(path, "/")
	var seg []string
	for _, s := range segments {
		if len(s) > 0 {
			seg = append(seg, s)
		}
	}

	jsonBody := func() (map[string]any, bool) {
		return parseJsonBody(raw)
	}
	badJson := func() {
		sendJson(w, 400, map[string]any{"message": "Problems parsing JSON"}, nil)
	}
	notFound := func() {
		sendJson(w, 404, map[string]any{"message": "Not Found"}, nil)
	}

	if path == "/api/v3" || path == "/api/v3/" {
		if r.Method != "GET" {
			notFound()
			return
		}
		sendJson(w, 200, map[string]any{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
		return
	}
	if path == "/api/v3/meta" && r.Method == "GET" {
		sendJson(w, 200, map[string]any{
			"installed_version": "1.0.0-forge",
			"api_versions":      []string{"3"},
		}, nil)
		return
	}
	if path == "/api/v3/user" && r.Method == "GET" {
		sendJson(w, 200, map[string]any{
			"login": ViewerLogin,
			"id":    ViewerID,
			"name":  ViewerName,
			"email": ViewerEmail,
			"type":  "User",
		}, nil)
		return
	}

	if path == "/api/v3/user/repos" {
		if r.Method == "GET" {
			var repoList []*Repo
			for _, rp := range repos {
				repoList = append(repoList, rp)
			}
			sort.Slice(repoList, func(i, j int) bool {
				return repoList[i].CreatedSeq < repoList[j].CreatedSeq
			})
			all := make([]any, len(repoList))
			for i, rp := range repoList {
				all[i] = repoJson(rp)
			}
			host := r.Host
			if host == "" {
				host = "localhost"
			}
			page, link := paginate(url, all, host)
			headers := map[string]string{}
			if link != "" {
				headers["link"] = link
			}
			sendJson(w, 200, page, headers)
			return
		}
		if r.Method == "POST" {
			body, ok := jsonBody()
			if !ok {
				badJson()
				return
			}
			name := ""
			if n, ok := body["name"].(string); ok {
				name = n
			}
			if name == "" {
				sendJson(w, 422, map[string]any{"message": "Validation Failed: name is required"}, nil)
				return
			}
			if _, exists := repos[name]; exists {
				sendJson(w, 422, map[string]any{"message": "Validation Failed: name already exists"}, nil)
				return
			}
			isPriv := false
			if p, ok := body["private"].(bool); ok {
				isPriv = p
			}
			desc := ""
			if d, ok := body["description"].(string); ok {
				desc = d
			}
			repo := newRepo(name, isPriv, desc)
			if db, ok := body["default_branch"].(string); ok && db != "" {
				repo.DefaultBranch = db
			}
			sendJson(w, 201, repoJson(repo), nil)
			return
		}
		notFound()
		return
	}

	// /api/v3/repos/{owner}/{repo}/...
	if len(seg) >= 5 && seg[0] == "api" && seg[1] == "v3" && seg[2] == "repos" {
		owner := pathUnescape(seg[3])
		repoName := pathUnescape(seg[4])
		repo := lookupRepo(owner, repoName)
		var rest []string
		for _, s := range seg[5:] {
			decoded := pathUnescape(s)
			rest = append(rest, decoded)
		}

		if len(rest) == 0 {
			if repo == nil {
				notFound()
				return
			}
			if r.Method == "GET" {
				sendJson(w, 200, repoJson(repo), nil)
				return
			}
			if r.Method == "PATCH" {
				body, ok := jsonBody()
				if !ok {
					badJson()
					return
				}
				if d, ok := body["description"].(string); ok {
					repo.Description = d
				}
				if p, ok := body["private"].(bool); ok {
					repo.IsPrivate = p
				}
				if db, ok := body["default_branch"].(string); ok && db != "" {
					repo.DefaultBranch = db
				}
				sendJson(w, 200, repoJson(repo), nil)
				return
			}
			if r.Method == "DELETE" {
				delete(repos, repoName)
				sendJson(w, 204, nil, nil)
				return
			}
			notFound()
			return
		}
		if repo == nil {
			notFound()
			return
		}

		if rest[0] == "branches" && r.Method == "GET" {
			var out []map[string]any
			for refName, sha := range repo.Refs {
				if strings.HasPrefix(refName, "refs/heads/") {
					bName := strings.TrimPrefix(refName, "refs/heads/")
					out = append(out, map[string]any{
						"name":      bName,
						"commit":    map[string]any{"sha": sha},
						"protected": false,
					})
				}
			}
			sort.Slice(out, func(i, j int) bool {
				return out[i]["name"].(string) < out[j]["name"].(string)
			})
			sendJson(w, 200, out, nil)
			return
		}
		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" && r.Method == "POST" {
			body, ok := jsonBody()
			if !ok {
				badJson()
				return
			}
			ref := ""
			if rf, ok := body["ref"].(string); ok {
				ref = rf
			}
			sha := ""
			if sh, ok := body["sha"].(string); ok {
				sha = sh
			}
			obj, hasObj := repo.Objects[sha]
			if ref == "" || !hasObj {
				sendJson(w, 422, map[string]any{"message": "Validation Failed"}, nil)
				return
			}
			repo.Refs[ref] = sha
			sendJson(w, 201, map[string]any{
				"ref": ref,
				"object": map[string]any{
					"sha":  sha,
					"type": obj.Type,
				},
			}, nil)
			return
		}
		if rest[0] == "commits" && r.Method == "GET" {
			shaParam := url.Query().Get("sha")
			var tip string
			if shaParam != "" {
				if bt := branchTip(repo, shaParam); bt != "" {
					tip = bt
				} else if _, has := repo.Objects[shaParam]; has {
					tip = shaParam
				}
			} else {
				tip = branchTip(repo, repo.DefaultBranch)
			}
			if tip == "" {
				sendJson(w, 200, []any{}, nil)
				return
			}
			commitShas := listCommits(repo, tip)
			allCommits := make([]any, len(commitShas))
			for i, s := range commitShas {
				allCommits[i] = commitJson(repo, s)
			}
			host := r.Host
			if host == "" {
				host = "localhost"
			}
			page, link := paginate(url, allCommits, host)
			headers := map[string]string{}
			if link != "" {
				headers["link"] = link
			}
			sendJson(w, 200, page, headers)
			return
		}
		if rest[0] == "contents" {
			filePath := strings.Join(rest[1:], "/")
			routeContents(w, repo, filePath, url, r.Method, raw)
			return
		}
		if rest[0] == "labels" {
			if r.Method == "POST" && len(rest) == 1 {
				body, ok := jsonBody()
				if !ok {
					badJson()
					return
				}
				lname := ""
				if ln, ok := body["name"].(string); ok {
					lname = ln
				}
				if lname == "" {
					sendJson(w, 422, map[string]any{"message": "Validation Failed"}, nil)
					return
				}
				color := "ededed"
				if cl, ok := body["color"].(string); ok && cl != "" {
					color = cl
				}
				repo.Labels[lname] = Label{Name: lname, Color: color}
				repo.LabelVer++
				descVal := any(nil)
				if dv, ok := body["description"]; ok {
					descVal = dv
				}
				sendJson(w, 201, map[string]any{
					"name":        lname,
					"color":       color,
					"description": descVal,
				}, nil)
				return
			}
			if r.Method == "GET" && len(rest) == 1 {
				var lbls []map[string]any
				for _, l := range repo.Labels {
					lbls = append(lbls, map[string]any{
						"name":  l.Name,
						"color": l.Color,
					})
				}
				sort.Slice(lbls, func(i, j int) bool {
					return lbls[i]["name"].(string) < lbls[j]["name"].(string)
				})
				sendJson(w, 200, lbls, nil)
				return
			}
			notFound()
			return
		}
		if rest[0] == "issues" {
			host := r.Host
			if host == "" {
				host = "localhost"
			}
			routeIssues(w, repo, rest, url, r.Method, raw, host)
			return
		}
		if rest[0] == "pulls" {
			host := r.Host
			if host == "" {
				host = "localhost"
			}
			routePulls(w, repo, rest, url, r.Method, raw, host)
			return
		}
		notFound()
		return
	}

	notFound()
}

func routeContents(w http.ResponseWriter, repo *Repo, filePath string, url *url.URL, method string, raw []byte) {
	notFound := func() {
		sendJson(w, 404, map[string]any{"message": "Not Found"}, nil)
	}
	if method == "GET" {
		ref := repo.DefaultBranch
		if rRef := url.Query().Get("ref"); rRef != "" {
			ref = rRef
		}
		tip := branchTip(repo, ref)
		if tip == "" {
			if _, has := repo.Objects[ref]; has {
				tip = ref
			}
		}
		if tip == "" {
			notFound()
			return
		}
		treeSha := treeOfCommit(repo, tip)
		if filePath == "" {
			treeObj, ok := repo.Objects[treeSha]
			treeData := []byte{}
			if ok {
				treeData = treeObj.Data
			}
			entriesRaw := parseTree(treeData)
			var entries []map[string]any
			for _, e := range entriesRaw {
				size := 0
				eType := "file"
				if e.Mode == "40000" {
					eType = "dir"
				} else {
					if bObj, hasB := repo.Objects[e.Sha]; hasB {
						size = len(bObj.Data)
					}
				}
				entries = append(entries, map[string]any{
					"name": e.Name,
					"path": e.Name,
					"sha":  e.Sha,
					"type": eType,
					"size": size,
				})
			}
			sendJson(w, 200, entries, nil)
			return
		}

		parts := strings.Split(filePath, "/")
		cur := treeSha
		var foundEntry *TreeEntry
		for _, p := range parts {
			obj, ok := repo.Objects[cur]
			if !ok || obj.Type != "tree" {
				foundEntry = nil
				break
			}
			entries := parseTree(obj.Data)
			var matched *TreeEntry
			for i := range entries {
				if entries[i].Name == p {
					matched = &entries[i]
					break
				}
			}
			foundEntry = matched
			if foundEntry == nil {
				break
			}
			cur = foundEntry.Sha
		}
		if foundEntry == nil {
			notFound()
			return
		}
		if foundEntry.Mode == "40000" {
			subObj, ok := repo.Objects[foundEntry.Sha]
			subData := []byte{}
			if ok {
				subData = subObj.Data
			}
			entriesRaw := parseTree(subData)
			var entries []map[string]any
			for _, e := range entriesRaw {
				size := 0
				eType := "file"
				if e.Mode == "40000" {
					eType = "dir"
				} else {
					if bObj, hasB := repo.Objects[e.Sha]; hasB {
						size = len(bObj.Data)
					}
				}
				entries = append(entries, map[string]any{
					"name": e.Name,
					"path": filePath + "/" + e.Name,
					"sha":  e.Sha,
					"type": eType,
					"size": size,
				})
			}
			sendJson(w, 200, entries, nil)
			return
		}
		blob, ok := repo.Objects[foundEntry.Sha]
		if !ok {
			notFound()
			return
		}
		name := parts[len(parts)-1]
		sendJson(w, 200, map[string]any{
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(blob.Data),
			"sha":      foundEntry.Sha,
			"size":     len(blob.Data),
			"name":     name,
			"path":     filePath,
		}, nil)
		return
	}

	body, ok := parseJsonBody(raw)
	if !ok {
		sendJson(w, 400, map[string]any{"message": "Problems parsing JSON"}, nil)
		return
	}
	branch := repo.DefaultBranch
	if b, ok := body["branch"].(string); ok && b != "" {
		branch = b
	}
	tip := branchTip(repo, branch)
	treeSha := treeOfCommit(repo, tip)
	current := blobAtPath(repo, treeSha, filePath)

	if method == "PUT" {
		var givenSha *string
		if gs, ok := body["sha"].(string); ok {
			givenSha = &gs
		}
		if current != "" && givenSha != nil && *givenSha != current {
			sendJson(w, 409, map[string]any{"message": "Conflict: sha does not match"}, nil)
			return
		}
		if current != "" && givenSha == nil {
			sendJson(w, 422, map[string]any{"message": "Validation Failed: sha is required to update"}, nil)
			return
		}
		contentStr := ""
		if cs, ok := body["content"].(string); ok {
			contentStr = cs
		}
		data, _ := base64.StdEncoding.DecodeString(contentStr)
		blobSha := storeObject(repo, "blob", data)
		newTree := updateTree(repo, treeSha, strings.Split(filePath, "/"), blobSha, false)
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		msg := "update"
		if m, ok := body["message"].(string); ok && m != "" {
			msg = m
		}
		commitSha := makeCommit(repo, newTree, parents, msg)
		repo.Refs["refs/heads/"+branch] = commitSha
		status := 200
		if current == "" {
			status = 201
		}
		fileName := filePath
		if idx := strings.LastIndex(filePath, "/"); idx != -1 {
			fileName = filePath[idx+1:]
		}
		sendJson(w, status, map[string]any{
			"content": map[string]any{
				"name":     fileName,
				"path":     filePath,
				"sha":      blobSha,
				"size":     len(data),
				"encoding": "base64",
			},
			"commit": map[string]any{
				"sha": commitSha,
			},
		}, nil)
		return
	}
	if method == "DELETE" {
		if current == "" {
			notFound()
			return
		}
		var givenSha *string
		if gs, ok := body["sha"].(string); ok {
			givenSha = &gs
		}
		if givenSha != nil && *givenSha != current {
			sendJson(w, 409, map[string]any{"message": "Conflict: sha does not match"}, nil)
			return
		}
		newTree := updateTree(repo, treeSha, strings.Split(filePath, "/"), "", true)
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		msg := "delete"
		if m, ok := body["message"].(string); ok && m != "" {
			msg = m
		}
		commitSha := makeCommit(repo, newTree, parents, msg)
		repo.Refs["refs/heads/"+branch] = commitSha
		sendJson(w, 200, map[string]any{
			"content": nil,
			"commit": map[string]any{
				"sha": commitSha,
			},
		}, nil)
		return
	}
	notFound()
}

func routeIssues(w http.ResponseWriter, repo *Repo, rest []string, url *url.URL, method string, raw []byte, host string) {
	notFound := func() {
		sendJson(w, 404, map[string]any{"message": "Not Found"}, nil)
	}
	if len(rest) == 1 {
		if method == "POST" {
			body, ok := parseJsonBody(raw)
			if !ok {
				sendJson(w, 400, map[string]any{"message": "Problems parsing JSON"}, nil)
				return
			}
			var labels []string
			if lbls, ok := body["labels"].([]any); ok {
				for _, item := range lbls {
					if s, ok := item.(string); ok {
						labels = append(labels, s)
					}
				}
			}
			for _, l := range labels {
				if _, exists := repo.Labels[l]; !exists {
					sendJson(w, 422, map[string]any{"message": "Validation Failed: label " + l + " does not exist"}, nil)
					return
				}
			}
			number := repo.NextNumber
			repo.NextNumber++
			issueID := nextIssueID
			nextIssueID++
			seqCounter++
			createdSeq := seqCounter

			title := ""
			if t, ok := body["title"].(string); ok {
				title = t
			}
			bodyStr := ""
			if b, ok := body["body"].(string); ok {
				bodyStr = b
			}

			rec := &IssueRec{
				Kind:       "issue",
				Number:     number,
				ID:         issueID,
				Title:      title,
				Body:       bodyStr,
				State:      "open",
				Labels:     labels,
				Comments:   []Comment{},
				CreatedSeq: createdSeq,
				Ver:        0,
			}
			repo.Issues = append(repo.Issues, rec)
			sendJson(w, 201, issueJson(repo, rec), nil)
			return
		}
		if method == "GET" {
			state := url.Query().Get("state")
			if state == "" {
				state = "open"
			}
			direction := url.Query().Get("direction")
			if direction == "" {
				direction = "asc"
			}
			var list []*IssueRec
			for _, i := range repo.Issues {
				if i.Kind == "issue" {
					if state == "all" || i.State == state {
						list = append(list, i)
					}
				}
			}
			sort.Slice(list, func(i, j int) bool {
				if direction == "desc" {
					return list[i].CreatedSeq > list[j].CreatedSeq
				}
				return list[i].CreatedSeq < list[j].CreatedSeq
			})
			mapped := make([]any, len(list))
			for i, rec := range list {
				mapped[i] = issueJson(repo, rec)
			}
			page, link := paginate(url, mapped, host)
			headers := map[string]string{}
			if link != "" {
				headers["link"] = link
			}
			sendJson(w, 200, page, headers)
			return
		}
		notFound()
		return
	}

	num, err := strconv.Atoi(rest[1])
	if err != nil {
		notFound()
		return
	}
	var rec *IssueRec
	for _, i := range repo.Issues {
		if i.Kind == "issue" && i.Number == num {
			rec = i
			break
		}
	}

	if len(rest) == 2 {
		if rec == nil {
			notFound()
			return
		}
		if method == "GET" {
			if rec.HasCache && rec.CacheVer == rec.Ver && rec.CacheLabelVer == repo.LabelVer {
				sendJsonText(w, 200, rec.CacheText, nil)
				return
			}
			text := marshalJson(issueJson(repo, rec)) + "\n"
			rec.CacheText = text
			rec.CacheVer = rec.Ver
			rec.CacheLabelVer = repo.LabelVer
			rec.HasCache = true
			sendJsonText(w, 200, text, nil)
			return
		}
		if method == "PATCH" {
			body, ok := parseJsonBody(raw)
			if !ok {
				sendJson(w, 400, map[string]any{"message": "Problems parsing JSON"}, nil)
				return
			}
			if st, ok := body["state"].(string); ok && (st == "closed" || st == "open") {
				rec.State = st
			}
			if t, ok := body["title"].(string); ok {
				rec.Title = t
			}
			if b, ok := body["body"].(string); ok {
				rec.Body = b
			}
			rec.Ver++
			sendJson(w, 200, issueJson(repo, rec), nil)
			return
		}
		notFound()
		return
	}

	if len(rest) == 3 && rest[2] == "comments" {
		if rec == nil {
			notFound()
			return
		}
		if method == "POST" {
			body, ok := parseJsonBody(raw)
			if !ok {
				sendJson(w, 400, map[string]any{"message": "Problems parsing JSON"}, nil)
				return
			}
			commentID := nextIssueID
			nextIssueID++
			bodyStr := ""
			if b, ok := body["body"].(string); ok {
				bodyStr = b
			}
			comment := Comment{ID: commentID, Body: bodyStr}
			rec.Comments = append(rec.Comments, comment)
			rec.Ver++
			sendJson(w, 201, map[string]any{
				"id":   comment.ID,
				"body": comment.Body,
				"user": map[string]any{"login": ViewerLogin, "id": ViewerID},
			}, nil)
			return
		}
		if method == "GET" {
			comms := make([]any, len(rec.Comments))
			for i, c := range rec.Comments {
				comms[i] = map[string]any{
					"id":   c.ID,
					"body": c.Body,
					"user": map[string]any{"login": ViewerLogin, "id": ViewerID},
				}
			}
			sendJson(w, 200, comms, nil)
			return
		}
		notFound()
		return
	}

	notFound()
}

func routePulls(w http.ResponseWriter, repo *Repo, rest []string, url *url.URL, method string, raw []byte, host string) {
	notFound := func() {
		sendJson(w, 404, map[string]any{"message": "Not Found"}, nil)
	}
	if len(rest) == 1 {
		if method == "POST" {
			body, ok := parseJsonBody(raw)
			if !ok {
				sendJson(w, 400, map[string]any{"message": "Problems parsing JSON"}, nil)
				return
			}
			headStr := ""
			if h, ok := body["head"].(string); ok {
				headStr = h
			}
			head := strings.TrimPrefix(headStr, ViewerLogin+":")
			base := ""
			if b, ok := body["base"].(string); ok {
				base = b
			}
			if base == "" {
				base = repo.DefaultBranch
			}
			if branchTip(repo, head) == "" {
				sendJson(w, 422, map[string]any{"message": "Validation Failed: head branch does not exist"}, nil)
				return
			}
			number := repo.NextNumber
			repo.NextNumber++
			issueID := nextIssueID
			nextIssueID++
			seqCounter++
			createdSeq := seqCounter

			title := ""
			if t, ok := body["title"].(string); ok {
				title = t
			}
			bodyStr := ""
			if b, ok := body["body"].(string); ok {
				bodyStr = b
			}

			rec := &IssueRec{
				Kind:           "pull",
				Number:         number,
				ID:             issueID,
				Title:          title,
				Body:           bodyStr,
				State:          "open",
				Labels:         []string{},
				Comments:       []Comment{},
				CreatedSeq:     createdSeq,
				Ver:            0,
				HeadRef:        head,
				BaseRef:        base,
				Merged:         false,
				MergeCommitSha: "",
			}
			repo.Issues = append(repo.Issues, rec)
			sendJson(w, 201, pullJson(repo, rec), nil)
			return
		}
		if method == "GET" {
			state := url.Query().Get("state")
			if state == "" {
				state = "open"
			}
			var list []*IssueRec
			for _, i := range repo.Issues {
				if i.Kind == "pull" {
					if state == "open" {
						if !i.Merged && i.State == "open" {
							list = append(list, i)
						}
					} else if state == "closed" {
						if i.Merged || i.State == "closed" {
							list = append(list, i)
						}
					} else {
						list = append(list, i)
					}
				}
			}
			mapped := make([]any, len(list))
			for i, rec := range list {
				mapped[i] = pullJson(repo, rec)
			}
			page, link := paginate(url, mapped, host)
			headers := map[string]string{}
			if link != "" {
				headers["link"] = link
			}
			sendJson(w, 200, page, headers)
			return
		}
		notFound()
		return
	}

	num, err := strconv.Atoi(rest[1])
	if err != nil {
		notFound()
		return
	}
	var rec *IssueRec
	for _, i := range repo.Issues {
		if i.Kind == "pull" && i.Number == num {
			rec = i
			break
		}
	}

	if len(rest) == 2 {
		if rec == nil {
			notFound()
			return
		}
		if method == "GET" {
			sendJson(w, 200, pullJson(repo, rec), nil)
			return
		}
		notFound()
		return
	}

	if len(rest) == 3 && rest[2] == "files" {
		if rec == nil {
			notFound()
			return
		}
		if method == "GET" {
			sendJson(w, 200, pullFiles(repo, rec), nil)
			return
		}
		notFound()
		return
	}

	if len(rest) == 3 && rest[2] == "merge" {
		if rec == nil {
			notFound()
			return
		}
		if method == "PUT" {
			if rec.Merged {
				sendJson(w, 405, map[string]any{"message": "Pull Request is not mergeable"}, nil)
				return
			}
			headSha := ""
			if rec.HeadRef != "" {
				headSha = branchTip(repo, rec.HeadRef)
			}
			baseSha := ""
			if rec.BaseRef != "" {
				baseSha = branchTip(repo, rec.BaseRef)
			}
			if headSha == "" || baseSha == "" {
				sendJson(w, 405, map[string]any{"message": "Pull Request is not mergeable"}, nil)
				return
			}
			mergedTree, ok := mergeTrees(repo, baseSha, headSha)
			if !ok {
				sendJson(w, 405, map[string]any{"message": "Pull Request is not mergeable: conflicts"}, nil)
				return
			}
			commitSha := makeCommit(repo, mergedTree, []string{baseSha}, rec.Title+" (#"+strconv.Itoa(rec.Number)+")")
			repo.Refs["refs/heads/"+rec.BaseRef] = commitSha
			rec.Merged = true
			rec.State = "closed"
			rec.MergeCommitSha = commitSha
			rec.HasMergeSha = true
			rec.MergedTs = nextTs()
			sendJson(w, 200, map[string]any{
				"sha":     commitSha,
				"merged":  true,
				"message": "Pull Request successfully merged",
			}, nil)
			return
		}
		notFound()
		return
	}

	notFound()
}
