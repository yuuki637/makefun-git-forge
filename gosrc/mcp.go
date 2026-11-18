package main

import (
	"encoding/json"
	"strings"
)

// MCP surface (ported from mcp.ts). The tools/list payload is produced
// from ordered structs so it stays byte-identical across calls.

var serverInfo = map[string]interface{}{"name": "git-forge", "version": "1.0.0"}

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpProp struct {
	Name string
	Type string
	Desc string
}

type mcpTool struct {
	Name     string
	Desc     string
	Props    []mcpProp
	Required []string
}

var mcpTools = []mcpTool{
	{
		Name: "create_repo", Desc: "Create a new repository owned by the authenticated user.",
		Props: []mcpProp{
			{"name", "string", "Repository name."},
			{"description", "string", "Repository description."},
			{"private", "boolean", "Whether the repository is private."},
		},
		Required: []string{"name"},
	},
	{
		Name: "get_repo", Desc: "Get a repository by owner and name.",
		Props: []mcpProp{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
		},
		Required: []string{"owner", "repo"},
	},
	{
		Name: "create_issue", Desc: "Create an issue in a repository.",
		Props: []mcpProp{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"title", "string", "Issue title."},
			{"body", "string", "Issue body."},
		},
		Required: []string{"owner", "repo", "title"},
	},
	{
		Name: "list_issues", Desc: "List issues in a repository.",
		Props: []mcpProp{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"state", "string", "Filter by state: open, closed or all."},
		},
		Required: []string{"owner", "repo"},
	},
	{
		Name: "create_pull_request", Desc: "Create a pull request from a head branch into a base branch.",
		Props: []mcpProp{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"title", "string", "Pull request title."},
			{"head", "string", "Head branch name."},
			{"base", "string", "Base branch name."},
			{"body", "string", "Pull request body."},
		},
		Required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		Name: "merge_pull_request", Desc: "Merge a pull request (squash) into its base branch.",
		Props: []mcpProp{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"pull_number", "number", "Pull request number."},
		},
		Required: []string{"owner", "repo", "pull_number"},
	},
}

// toolsListJSON renders the tools array exactly like JSON.stringify of
// the Node TOOLS constant (same key order).
func toolsListJSON() []byte {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, t := range mcpTools {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"name":`)
		sb.Write(marshalJSONString(t.Name))
		sb.WriteString(`,"description":`)
		sb.Write(marshalJSONString(t.Desc))
		sb.WriteString(`,"inputSchema":{"type":"object","properties":{`)
		for j, p := range t.Props {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.Write(marshalJSONString(p.Name))
			sb.WriteString(`:{"type":`)
			sb.Write(marshalJSONString(p.Type))
			sb.WriteString(`,"description":`)
			sb.Write(marshalJSONString(p.Desc))
			sb.WriteByte('}')
		}
		sb.WriteString(`},"required":[`)
		for j, r := range t.Required {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.Write(marshalJSONString(r))
		}
		sb.WriteString(`]}}`)
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

func marshalJSONString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

func findTool(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

type rpcFailure struct {
	code    int
	message string
}

func (e *rpcFailure) Error() string { return e.message }

type rpcOutcome struct {
	status  int
	payload interface{}
	hasPay  bool
	empty   bool
}

func rpcResult(id interface{}, result interface{}) rpcOutcome {
	return rpcOutcome{status: 200, payload: orderedRPC(id, "result", result), hasPay: true}
}

func rpcError(id interface{}, code int, message string, status int) rpcOutcome {
	errObj := newOMap()
	errObj.set("code", code)
	errObj.set("message", message)
	return rpcOutcome{status: status, payload: orderedRPCWithError(id, errObj), hasPay: true}
}

// RPC envelopes use OMap to preserve the JS key order (jsonrpc, id, result/error).
func orderedRPC(id interface{}, key string, value interface{}) *OMap {
	m := newOMap()
	m.set("jsonrpc", "2.0")
	m.set("id", id)
	m.set(key, value)
	return m
}

func orderedRPCWithError(id interface{}, errObj *OMap) *OMap {
	m := newOMap()
	m.set("jsonrpc", "2.0")
	m.set("id", id)
	m.set("error", errObj)
	return m
}

func toolOk(value map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"content":           []interface{}{map[string]interface{}{"type": "text", "text": string(marshalJSON(value))}},
		"structuredContent": value,
		"isError":           false,
	}
}

func toolErr(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": message}},
		"isError": true,
	}
}

func argStr(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return ""
}

func callTool(store *Store, name string, rawArgs interface{}) (map[string]interface{}, error) {
	args, _ := rawArgs.(map[string]interface{})
	if args == nil {
		args = map[string]interface{}{}
	}
	tool := findTool(name)
	if tool == nil {
		return nil, &rpcFailure{-32602, "Unknown tool: " + name}
	}
	for _, required := range tool.Required {
		v, present := args[required]
		if !present || v == nil || v == "" {
			return nil, &rpcFailure{-32602, "Missing required argument: " + required}
		}
	}
	owner := argStr(args, "owner")
	repoName := argStr(args, "repo")
	resolve := func() *Repo {
		if owner == "" || repoName == "" {
			return nil
		}
		return store.resolveRepo(owner, repoName)
	}

	switch name {
	case "create_repo":
		repoNameArg := argStr(args, "name")
		if existing, ok := store.ReposByName[repoNameArg]; ok {
			effectiveOwner := owner
			if effectiveOwner == "" {
				effectiveOwner = store.viewerLogin()
			}
			if store.viewerLogin() == effectiveOwner {
				return toolOk(repoJSON(store, existing)), nil
			}
			return toolErr("Repository " + repoNameArg + " already exists"), nil
		}
		var desc *string
		if d, ok := args["description"].(string); ok {
			desc = &d
		}
		repo := store.createRepo(repoNameArg, args["private"] == true, desc)
		return toolOk(repoJSON(store, repo)), nil
	case "get_repo":
		repo := resolve()
		if repo == nil {
			return toolErr("Repository not found"), nil
		}
		return toolOk(repoJSON(store, repo)), nil
	case "create_issue":
		repo := resolve()
		if repo == nil {
			return toolErr("Repository not found"), nil
		}
		issue := createIssue(store, repo, argStr(args, "title"), argStr(args, "body"), nil)
		return toolOk(issueJSON(store, repo, issue)), nil
	case "list_issues":
		repo := resolve()
		if repo == nil {
			return toolErr("Repository not found"), nil
		}
		state := argStr(args, "state")
		if state == "" {
			state = "all"
		}
		var list []*Issue
		for _, i := range repo.Issues {
			list = append(list, i)
		}
		sortIssuesAsc(list)
		if state == "open" || state == "closed" {
			list = filterIssues(list, state)
		}
		issues := make([]interface{}, 0, len(list))
		for _, i := range list {
			issues = append(issues, issueJSON(store, repo, i))
		}
		return toolOk(map[string]interface{}{"issues": issues}), nil
	case "create_pull_request":
		repo := resolve()
		if repo == nil {
			return toolErr("Repository not found"), nil
		}
		head := lastColonPart(argStr(args, "head"))
		base := lastColonPart(argStr(args, "base"))
		if _, ok := repo.Refs["refs/heads/"+head]; !ok {
			return toolErr("Head branch \"" + head + "\" does not exist"), nil
		}
		if _, ok := repo.Refs["refs/heads/"+base]; !ok {
			return toolErr("Base branch \"" + base + "\" does not exist"), nil
		}
		var existing *Pull
		for _, p := range repo.Pulls {
			if p.State == "open" && p.Head == head && p.Base == base {
				existing = p
				break
			}
		}
		pull := existing
		if pull == nil {
			pull = createPull(store, repo, argStr(args, "title"), argStr(args, "body"), head, base)
		}
		return toolOk(pullJSON(store, repo, pull)), nil
	case "merge_pull_request":
		repo := resolve()
		if repo == nil {
			return toolErr("Repository not found"), nil
		}
		numberRaw, present := args["pull_number"]
		if !present {
			numberRaw = args["number"]
		}
		numFloat, isNum := jsNumber(numberRaw)
		var pull *Pull
		if isNum && numFloat == float64(int(numFloat)) {
			pull = repo.Pulls[int(numFloat)]
		}
		if pull == nil {
			return toolErr("Pull request not found"), nil
		}
		if pull.Merged {
			out := pullJSON(store, repo, pull)
			out["merged"] = true
			return toolOk(out), nil
		}
		commit, outcome := mergePull(store, repo, pull, "squash")
		if outcome == "conflict" {
			return toolErr("Pull request is not mergeable: merge conflict"), nil
		}
		if outcome == "null" {
			return toolErr("Pull request is not mergeable"), nil
		}
		out := pullJSON(store, repo, pull)
		out["merged"] = true
		out["sha"] = commit
		return toolOk(out), nil
	}
	return nil, &rpcFailure{-32602, "Unknown tool: " + name}
}

// lastColonPart mirrors JS s.split(':').pop().
func lastColonPart(s string) string {
	if idx := strings.LastIndex(s, ":"); idx != -1 {
		return s[idx+1:]
	}
	return s
}

func dispatchRPC(store *Store, message map[string]interface{}) rpcOutcome {
	method, _ := message["method"].(string)
	idVal, hasIDKey := message["id"]
	hasID := hasIDKey // JSON cannot produce undefined; key presence is what JS checks
	var id interface{}
	if hasID {
		id = idVal
	}
	params, _ := message["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
	}

	if !hasID {
		return rpcOutcome{status: 202, empty: true}
	}

	switch method {
	case "server/discover":
		result := newOMap()
		result.set("resultType", "complete")
		versions := make([]interface{}, len(supportedVersions))
		for i, v := range supportedVersions {
			versions[i] = v
		}
		result.set("supportedVersions", versions)
		result.set("capabilities", map[string]interface{}{"tools": map[string]interface{}{}})
		result.set("serverInfo", serverInfo)
		result.set("_meta", map[string]interface{}{"io.modelcontextprotocol/serverInfo": serverInfo})
		return rpcResult(id, result)
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		protocolVersion := "2025-11-25"
		for _, v := range supportedVersions {
			if v == requested {
				protocolVersion = requested
				break
			}
		}
		result := newOMap()
		result.set("protocolVersion", protocolVersion)
		result.set("capabilities", map[string]interface{}{"tools": map[string]interface{}{}})
		result.set("serverInfo", serverInfo)
		return rpcResult(id, result)
	case "ping":
		return rpcResult(id, newOMap())
	case "tools/list":
		result := newOMap()
		result.set("tools", json.RawMessage(toolsListJSON()))
		return rpcResult(id, result)
	case "tools/call":
		toolName, _ := params["name"].(string)
		callArgs := params["arguments"]
		if callArgs == nil {
			callArgs = map[string]interface{}{}
		}
		result, err := callTool(store, toolName, callArgs)
		if err != nil {
			if failure, ok := err.(*rpcFailure); ok {
				return rpcError(id, failure.code, failure.message, 200)
			}
			return rpcError(id, -32603, err.Error(), 200)
		}
		return rpcResult(id, result)
	default:
		return rpcError(id, -32601, "Method not found: "+method, 200)
	}
}

func handleMcp(ctx *Ctx) Reply {
	store := ctx.Store
	if ctx.Method != "POST" {
		return errorReply(405, "Method Not Allowed")
	}
	if ctx.JSONError {
		return jsonReply(400, rpcErrorPayload(nil, -32700, "Parse error"), nil)
	}
	if batch, ok := ctx.JSON.([]interface{}); ok {
		var payloads []interface{}
		for _, item := range batch {
			msg, _ := item.(map[string]interface{})
			if msg == nil {
				msg = map[string]interface{}{}
			}
			outcome := dispatchRPC(store, msg)
			if !outcome.empty {
				payloads = append(payloads, outcome.payload)
			}
		}
		if len(payloads) == 0 {
			return Reply{Status: 202, Headers: map[string]string{}, Body: []byte{}, HasBody: true}
		}
		return jsonReply(200, payloads, nil)
	}
	msg, ok := ctx.JSON.(map[string]interface{})
	if !ok {
		return jsonReply(400, rpcErrorPayload(nil, -32700, "Parse error"), nil)
	}
	outcome := dispatchRPC(store, msg)
	if outcome.empty {
		return Reply{Status: outcome.status, Headers: map[string]string{}, Body: []byte{}, HasBody: true}
	}
	return jsonReply(outcome.status, outcome.payload, nil)
}

func rpcErrorPayload(id interface{}, code int, message string) *OMap {
	errObj := newOMap()
	errObj.set("code", code)
	errObj.set("message", message)
	return orderedRPCWithError(id, errObj)
}
