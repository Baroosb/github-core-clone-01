package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const EpochSec int64 = 1735689600

const (
	ViewerID    = 1000
	ViewerLogin = "adaiversen"
	ViewerName  = "ada iversen"
	ViewerEmail = "adaiversen@example.invalid"
)

var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

var clockTick int64

func nextTs() int64 {
	return atomic.AddInt64(&clockTick, 1) + EpochSec
}

func isoOf(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

var Mu sync.RWMutex

var repos = map[string]*Repo{}

var (
	nextRepoID  = 5000
	nextIssueID = 100000
	seqCounter  = 0
)

type TreeEntry struct {
	Mode string
	Name string
	Sha  string
}

type CommitInfo struct {
	Tree          string
	Parents       []string
	Message       string
	AuthorLine    string
	CommitterLine string
}

type Comment struct {
	ID   int
	Body string
}

type IssueRec struct {
	Kind           string
	Number         int
	ID             int
	Title          string
	Body           string
	State          string
	Labels         []string
	Comments       []Comment
	Ver            int
	CacheText      string
	CacheVer       int
	CacheLabelVer  int
	HasCache       bool
	HeadRef        string
	BaseRef        string
	Merged         bool
	MergeCommitSha string
	HasMergeSha    bool
	MergedTs       int64
	CreatedSeq     int
}

type Label struct {
	Name  string
	Color string
}

type Repo struct {
	ID            int
	Name          string
	IsPrivate     bool
	Description   string
	DefaultBranch string
	Objects       map[string]GitObj
	Refs          map[string]string
	Labels        map[string]Label
	LabelVer      int
	Issues        []*IssueRec
	NextNumber    int
	CreatedSeq    int
	Ver           int
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], 0x20)
		if sp == -1 {
			break
		}
		sp += i
		mode := string(data[i:sp])
		nul := bytes.IndexByte(data[sp:], 0x00)
		if nul == -1 {
			break
		}
		nul += sp
		name := string(data[sp+1 : nul])
		if nul+1+20 > len(data) {
			break
		}
		sha := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, Sha: sha})
		i = nul + 21
	}
	return out
}

func serializeTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		an := sorted[i].Name
		if sorted[i].Mode == "40000" {
			an += "/"
		}
		bn := sorted[j].Name
		if sorted[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0x00)
		shaBytes, _ := hex.DecodeString(e.Sha)
		buf.Write(shaBytes)
	}
	return buf.Bytes()
}

func parseCommit(data []byte) CommitInfo {
	text := string(data)
	split := strings.Index(text, "\n\n")
	var head, message string
	if split == -1 {
		head = text
		message = ""
	} else {
		head = text[:split]
		message = text[split+2:]
	}
	var tree string
	var parents []string
	var authorLine, committerLine string
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		} else if strings.HasPrefix(line, "author ") {
			authorLine = line[7:]
		} else if strings.HasPrefix(line, "committer ") {
			committerLine = line[10:]
		}
	}
	return CommitInfo{
		Tree:          tree,
		Parents:       parents,
		Message:       message,
		AuthorLine:    authorLine,
		CommitterLine: committerLine,
	}
}

func newRepo(name string, isPrivate bool, description string) *Repo {
	seqCounter++
	repo := &Repo{
		ID:            nextRepoID,
		Name:          name,
		IsPrivate:     isPrivate,
		Description:   description,
		DefaultBranch: "main",
		Objects:       make(map[string]GitObj),
		Refs:          make(map[string]string),
		Labels:        make(map[string]Label),
		LabelVer:      0,
		Issues:        []*IssueRec{},
		NextNumber:    1,
		CreatedSeq:    seqCounter,
	}
	nextRepoID++
	repos[name] = repo
	return repo
}

func storeObject(repo *Repo, t ObjType, data []byte) string {
	sha := sha1Of(t, data)
	if _, ok := repo.Objects[sha]; !ok {
		repo.Objects[sha] = GitObj{Type: t, Data: data}
		repo.Ver++
	}
	return sha
}

func makeCommit(repo *Repo, tree string, parents []string, message string) string {
	ts := nextTs()
	ident := ViewerName + " <" + ViewerEmail + "> " + strconv.FormatInt(ts, 10) + " +0000"
	var buf bytes.Buffer
	buf.WriteString("tree ")
	buf.WriteString(tree)
	buf.WriteByte('\n')
	for _, p := range parents {
		buf.WriteString("parent ")
		buf.WriteString(p)
		buf.WriteByte('\n')
	}
	buf.WriteString("author ")
	buf.WriteString(ident)
	buf.WriteByte('\n')
	buf.WriteString("committer ")
	buf.WriteString(ident)
	buf.WriteString("\n\n")
	buf.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		buf.WriteByte('\n')
	}
	return storeObject(repo, ObjCommit, buf.Bytes())
}

func treeOfCommit(repo *Repo, commitSha string) string {
	if commitSha == "" {
		return storeObject(repo, ObjTree, []byte{})
	}
	obj, ok := repo.Objects[commitSha]
	if !ok || obj.Type != ObjCommit {
		return storeObject(repo, ObjTree, []byte{})
	}
	return parseCommit(obj.Data).Tree
}

func blobAtPath(repo *Repo, treeSha string, path string) string {
	parts := strings.Split(path, "/")
	cur := treeSha
	for i, part := range parts {
		obj, ok := repo.Objects[cur]
		if !ok || obj.Type != ObjTree {
			return ""
		}
		entries := parseTree(obj.Data)
		var found *TreeEntry
		for _, e := range entries {
			if e.Name == part {
				found = &e
				break
			}
		}
		if found == nil {
			return ""
		}
		if i == len(parts)-1 {
			if found.Mode == "40000" {
				return ""
			}
			return found.Sha
		}
		cur = found.Sha
	}
	return ""
}

func flattenTree(repo *Repo, treeSha string, prefix string) map[string]string {
	out := make(map[string]string)
	obj, ok := repo.Objects[treeSha]
	if !ok || obj.Type != ObjTree {
		return out
	}
	for _, e := range parseTree(obj.Data) {
		if e.Mode == "40000" {
			sub := flattenTree(repo, e.Sha, prefix+e.Name+"/")
			for k, v := range sub {
				out[k] = v
			}
		} else {
			out[prefix+e.Name] = e.Sha
		}
	}
	return out
}

func updateTree(repo *Repo, treeSha string, parts []string, blobSha string, remove bool) string {
	obj, ok := repo.Objects[treeSha]
	var entries []TreeEntry
	if ok && obj.Type == ObjTree {
		entries = parseTree(obj.Data)
	}
	head := parts[0]
	rest := parts[1:]

	idx := -1
	for i, e := range entries {
		if e.Name == head {
			idx = i
			break
		}
	}

	if len(rest) == 0 {
		if remove {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx] = TreeEntry{Mode: "100644", Name: head, Sha: blobSha}
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: head, Sha: blobSha})
			}
		}
	} else {
		subSha := ""
		if idx >= 0 && entries[idx].Mode == "40000" {
			subSha = entries[idx].Sha
		} else {
			subSha = storeObject(repo, ObjTree, []byte{})
		}
		newSub := updateTree(repo, subSha, rest, blobSha, remove)
		subObj, subOk := repo.Objects[newSub]
		empty := subOk && subObj.Type == ObjTree && len(subObj.Data) == 0
		if empty {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx] = TreeEntry{Mode: "40000", Name: head, Sha: newSub}
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: head, Sha: newSub})
			}
		}
	}
	return storeObject(repo, ObjTree, serializeTree(entries))
}

func branchTip(repo *Repo, branch string) string {
	return repo.Refs["refs/heads/"+branch]
}

func isAncestor(repo *Repo, anc string, desc string) bool {
	seen := make(map[string]bool)
	stack := []string{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj, ok := repo.Objects[cur]
		if ok && obj.Type == ObjCommit {
			info := parseCommit(obj.Data)
			stack = append(stack, info.Parents...)
		}
	}
	return false
}

func mergeBase(repo *Repo, a string, b string) (string, bool) {
	ancOf := func(start string) map[string]bool {
		out := make(map[string]bool)
		stack := []string{start}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if out[cur] {
				continue
			}
			out[cur] = true
			obj, ok := repo.Objects[cur]
			if ok && obj.Type == ObjCommit {
				info := parseCommit(obj.Data)
				stack = append(stack, info.Parents...)
			}
		}
		return out
	}
	aa := ancOf(a)
	seen := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if aa[cur] {
			return cur, true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj, ok := repo.Objects[cur]
		if ok && obj.Type == ObjCommit {
			info := parseCommit(obj.Data)
			queue = append(queue, info.Parents...)
		}
	}
	return "", false
}

func mergeTrees(repo *Repo, baseTip string, headTip string) (string, bool) {
	mb, ok := mergeBase(repo, baseTip, headTip)
	var baseFiles, headFiles, baseBranchFiles map[string]string
	if !ok {
		baseFiles = make(map[string]string)
	} else {
		baseFiles = flattenTree(repo, treeOfCommit(repo, mb), "")
	}
	headFiles = flattenTree(repo, treeOfCommit(repo, headTip), "")
	baseBranchFiles = flattenTree(repo, treeOfCommit(repo, baseTip), "")

	merged := make(map[string]string)
	pathsMap := make(map[string]bool)
	for k := range baseFiles {
		pathsMap[k] = true
	}
	for k := range headFiles {
		pathsMap[k] = true
	}
	for k := range baseBranchFiles {
		pathsMap[k] = true
	}

	for p := range pathsMap {
		inBase := baseFiles[p]
		inHead := headFiles[p]
		inBranch := baseBranchFiles[p]

		if inHead == inBranch {
			if inHead != "" {
				merged[p] = inHead
			}
			continue
		}
		if inBase == inBranch {
			if inHead != "" {
				merged[p] = inHead
			}
			continue
		}
		if inBase == inHead {
			if inBranch != "" {
				merged[p] = inBranch
			}
			continue
		}
		return "", false
	}

	tree := storeObject(repo, ObjTree, []byte{})
	for p, sha := range merged {
		parts := strings.Split(p, "/")
		tree = updateTree(repo, tree, parts, sha, false)
	}
	return tree, true
}
