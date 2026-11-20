package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

var mcpProtocolVersions = []string{"2026-07-28", "2025-06-18", "2025-03-26", "2024-11-05"}

func mcpToolsList() []map[string]any {
	obj := func(properties map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required}
	}
	str := map[string]any{"type": "string"}
	return []map[string]any{
		{"name": "create_repo", "description": "Create a new repository owned by the authenticated user.", "inputSchema": obj(map[string]any{"name": str, "description": str, "private": map[string]any{"type": "boolean"}}, []string{"name"})},
		{"name": "get_repo", "description": "Get a repository by owner and name.", "inputSchema": obj(map[string]any{"owner": str, "repo": str}, []string{"owner", "repo"})},
		{"name": "create_issue", "description": "Create an issue in a repository.", "inputSchema": obj(map[string]any{"owner": str, "repo": str, "title": str, "body": str}, []string{"owner", "repo", "title"})},
		{"name": "list_issues", "description": "List issues in a repository.", "inputSchema": obj(map[string]any{"owner": str, "repo": str, "state": str}, []string{"owner", "repo"})},
		{"name": "create_pull_request", "description": "Open a pull request from a head branch into a base branch.", "inputSchema": obj(map[string]any{"owner": str, "repo": str, "title": str, "head": str, "base": str, "body": str}, []string{"owner", "repo", "title", "head", "base"})},
		{"name": "merge_pull_request", "description": "Merge an open pull request.", "inputSchema": obj(map[string]any{"owner": str, "repo": str, "pull_number": map[string]any{"type": "number"}}, []string{"owner", "repo", "pull_number"})},
	}
}

func mcpToolResult(data any) map[string]any {
	var structured any
	if obj, ok := data.(map[string]any); ok {
		structured = obj
	} else {
		structured = map[string]any{"result": data}
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": marshalJson(data)}},
		"structuredContent": structured,
		"isError":           false,
	}
}

func mcpToolError(message string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	}
}

// jsNumberString renders a JSON number the way JS String(number) would:
// integers without a decimal point or exponent, others in shortest form.
func jsNumberString(n json.Number) string {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return string(n)
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsString mirrors JS String(v) for JSON-decoded values.
func jsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return jsNumberString(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if e == nil {
				parts[i] = ""
			} else {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// jsBool mirrors JS Boolean(v) for JSON-decoded values.
func jsBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case json.Number:
		f, err := t.Float64()
		return err == nil && f != 0
	case string:
		return t != ""
	default:
		return true
	}
}

// jsNumber mirrors JS Number(v) for JSON-decoded values.
func jsNumber(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := strconv.ParseFloat(string(t), 64)
		return f
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	case nil:
		return 0
	default:
		return math.NaN()
	}
}

// argOrEmpty mirrors String(args[key] ?? ”).
func argOrEmpty(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	return jsString(v)
}

// strArg mirrors `typeof args[key] === 'string' ? args[key] : ”`.
func strArg(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

type mcpCallOutcome struct {
	result  map[string]any
	errCode int
	errMsg  string
	isErr   bool
}

// mcpCallTool dispatches one tools/call. The caller must hold Mu: the tools
// mutate the store, and repoOf uses the lock-free lookupRepo (findRepo would
// deadlock on the held write lock).
func mcpCallTool(name string, args map[string]any) mcpCallOutcome {
	var tool map[string]any
	for _, t := range mcpToolsList() {
		if t["name"] == name {
			tool = t
			break
		}
	}
	if tool == nil {
		return mcpCallOutcome{errCode: -32602, errMsg: "unknown tool: " + name, isErr: true}
	}
	var required []string
	if schema, ok := tool["inputSchema"].(map[string]any); ok {
		if req, ok := schema["required"].([]string); ok {
			required = req
		}
	}
	for _, req := range required {
		v, present := args[req]
		if !present || v == nil || v == "" {
			return mcpCallOutcome{errCode: -32602, errMsg: "missing required argument: " + req, isErr: true}
		}
	}
	repoOf := func() *Repo {
		return lookupRepo(argOrEmpty(args, "owner"), argOrEmpty(args, "repo"))
	}
	switch name {
	case "create_repo":
		repo := newRepo(jsString(args["name"]), jsBool(args["private"]), strArg(args, "description"))
		return mcpCallOutcome{result: mcpToolResult(repoJson(repo))}
	case "get_repo":
		repo := repoOf()
		if repo == nil {
			return mcpCallOutcome{result: mcpToolError("repository not found")}
		}
		return mcpCallOutcome{result: mcpToolResult(repoJson(repo))}
	case "create_issue":
		repo := repoOf()
		if repo == nil {
			return mcpCallOutcome{result: mcpToolError("repository not found")}
		}
		number := repo.NextNumber
		repo.NextNumber++
		issueID := nextIssueID
		nextIssueID++
		seqCounter++
		rec := &IssueRec{
			Kind:       "issue",
			Number:     number,
			ID:         issueID,
			Title:      jsString(args["title"]),
			Body:       strArg(args, "body"),
			State:      "open",
			Labels:     []string{},
			Comments:   []Comment{},
			CreatedSeq: seqCounter,
			Ver:        0,
		}
		repo.Issues = append(repo.Issues, rec)
		return mcpCallOutcome{result: mcpToolResult(issueJson(repo, rec))}
	case "list_issues":
		repo := repoOf()
		if repo == nil {
			return mcpCallOutcome{result: mcpToolError("repository not found")}
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = s
		}
		list := make([]any, 0)
		for _, rec := range repo.Issues {
			if rec.Kind != "issue" {
				continue
			}
			if state == "open" && rec.State != "open" {
				continue
			}
			if state == "closed" && rec.State != "closed" {
				continue
			}
			list = append(list, issueJson(repo, rec))
		}
		return mcpCallOutcome{result: mcpToolResult(list)}
	case "create_pull_request":
		repo := repoOf()
		if repo == nil {
			return mcpCallOutcome{result: mcpToolError("repository not found")}
		}
		head := strings.TrimPrefix(jsString(args["head"]), ViewerLogin+":")
		base := jsString(args["base"])
		if branchTip(repo, head) == "" {
			return mcpCallOutcome{result: mcpToolError("head branch does not exist")}
		}
		if branchTip(repo, base) == "" {
			return mcpCallOutcome{result: mcpToolError("base branch does not exist")}
		}
		number := repo.NextNumber
		repo.NextNumber++
		issueID := nextIssueID
		nextIssueID++
		seqCounter++
		rec := &IssueRec{
			Kind:       "pull",
			Number:     number,
			ID:         issueID,
			Title:      jsString(args["title"]),
			Body:       strArg(args, "body"),
			State:      "open",
			Labels:     []string{},
			Comments:   []Comment{},
			CreatedSeq: seqCounter,
			Ver:        0,
			HeadRef:    head,
			BaseRef:    base,
			Merged:     false,
		}
		repo.Issues = append(repo.Issues, rec)
		return mcpCallOutcome{result: mcpToolResult(pullJson(repo, rec))}
	case "merge_pull_request":
		repo := repoOf()
		if repo == nil {
			return mcpCallOutcome{result: mcpToolError("repository not found")}
		}
		num := jsNumber(args["pull_number"])
		var rec *IssueRec
		for _, i := range repo.Issues {
			if i.Kind == "pull" && float64(i.Number) == num {
				rec = i
				break
			}
		}
		if rec == nil {
			return mcpCallOutcome{result: mcpToolError("pull request not found")}
		}
		sha := mergePullRec(repo, rec)
		if sha == nil {
			return mcpCallOutcome{result: mcpToolError("pull request is not mergeable")}
		}
		merged := pullJson(repo, rec)
		merged["sha"] = sha
		merged["merged"] = true
		return mcpCallOutcome{result: mcpToolResult(merged)}
	}
	return mcpCallOutcome{errCode: -32602, errMsg: "unknown tool: " + name, isErr: true}
}

// mcpHandleOne handles one JSON-RPC message; the second return value is
// false when the message is a notification the TS answers with silence.
func mcpHandleOne(msg map[string]any) (map[string]any, bool) {
	id := msg["id"]
	_, hasID := msg["id"]
	isNotification := !hasID || id == nil
	method, _ := msg["method"].(string)
	ok := func(result any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	}
	fail := func(code int, message string) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
	}
	switch method {
	case "server/discover":
		return ok(map[string]any{
			"_meta":             map[string]any{},
			"protocolVersion":   "2026-07-28",
			"supportedVersions": mcpProtocolVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge-mcp", "version": "1.0.0"},
		}), true
	case "initialize":
		wanted := ""
		if params, ok := msg["params"].(map[string]any); ok {
			if pv, ok := params["protocolVersion"].(string); ok {
				wanted = pv
			}
		}
		version := "2025-06-18"
		for _, v := range mcpProtocolVersions {
			if v == wanted {
				version = wanted
				break
			}
		}
		return ok(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "forge-mcp", "version": "1.0.0"},
		}), true
	case "ping":
		return ok(map[string]any{}), true
	case "tools/list":
		return ok(map[string]any{"tools": mcpToolsList()}), true
	case "tools/call":
		var params map[string]any
		if p, ok := msg["params"].(map[string]any); ok {
			params = p
		}
		name, isStr := "", false
		if params != nil {
			name, isStr = params["name"].(string)
		}
		if !isStr {
			return fail(-32602, "missing tool name"), true
		}
		toolArgs := map[string]any{}
		if a, ok := params["arguments"].(map[string]any); ok {
			toolArgs = a
		}
		outcome := mcpCallTool(name, toolArgs)
		if outcome.isErr {
			return fail(outcome.errCode, outcome.errMsg), true
		}
		return ok(outcome.result), true
	default:
		if isNotification {
			return nil, false
		}
		return fail(-32601, "method not found: "+method), true
	}
}

// mcpWriteRaw writes a response the way the TS handleMcp writes its
// non-sendJson answers: JSON content type, exact body, no envelope extras.
func mcpWriteRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body != "" {
		w.Write([]byte(body))
	}
}

func handleMcp(w http.ResponseWriter, raw []byte) {
	Mu.Lock()
	defer Mu.Unlock()

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed any
	parseFailed := false
	if err := dec.Decode(&parsed); err != nil {
		parseFailed = true
	} else {
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			parseFailed = true
		}
	}
	if parseFailed {
		mcpWriteRaw(w, 400, marshalJson(map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32700, "message": "Parse error"},
		}))
		return
	}
	if arr, isArr := parsed.([]any); isArr {
		out := make([]any, 0, len(arr))
		for _, el := range arr {
			obj, isObj := el.(map[string]any)
			if !isObj || obj == nil {
				continue
			}
			if resp, has := mcpHandleOne(obj); has {
				out = append(out, resp)
			}
		}
		sendJson(w, 200, out, nil)
		return
	}
	obj, isObj := parsed.(map[string]any)
	if !isObj || obj == nil {
		mcpWriteRaw(w, 400, marshalJson(map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32600, "message": "Invalid Request"},
		}))
		return
	}
	resp, has := mcpHandleOne(obj)
	if !has {
		mcpWriteRaw(w, 202, "")
		return
	}
	sendJson(w, 200, resp, nil)
}
