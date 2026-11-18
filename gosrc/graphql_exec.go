package main

import (
	"sort"
	"strings"
)

// GraphQL schema tables, node resolution and execution (ported from
// graphql.ts).

var typeFields = map[string][]string{
	"Query":    {"viewer", "repository", "node", "user", "organization", "search", "rateLimit", "__type", "__schema"},
	"Mutation": {"createIssue", "closeIssue", "reopenIssue", "updateIssue", "createPullRequest", "mergePullRequest", "addComment", "addLabelsToLabelable", "removeLabelsFromLabelable", "updatePullRequest", "closePullRequest"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
		"hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "description", "isPrivate",
		"url", "createdAt", "updatedAt", "stargazerCount", "forkCount", "isArchived", "isFork", "parent",
		"primaryLanguage", "licenseInfo", "defaultBranch", "refs", "ref", "object", "viewerCanAdminister",
	},
	"Issue": {
		"id", "number", "title", "state", "body", "author", "labels", "comments", "createdAt", "updatedAt",
		"url", "repository", "closed", "stateReason", "viewerCanUpdate", "assignees", "milestone",
	},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName",
		"files", "author", "createdAt", "updatedAt", "closed", "mergedAt", "mergeCommit", "baseRef",
		"headRef", "additions", "deletions", "changedFiles", "url", "repository", "headRefOid", "baseRefOid",
	},
	"User":                          {"id", "login", "name", "email", "url", "avatarUrl", "databaseId"},
	"Ref":                           {"id", "name", "prefix", "target", "repository"},
	"Commit":                        {"id", "oid", "message", "tree"},
	"Tree":                          {"id", "oid", "entries"},
	"PageInfo":                      {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"IssueConnection":               {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":                     {"node", "cursor"},
	"PullRequestConnection":         {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestEdge":               {"node", "cursor"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":        {"path", "additions", "deletions", "changeType"},
	"Label":                         {"id", "name", "color", "description"},
	"LabelConnection":               {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueComment":                  {"id", "body", "author", "createdAt", "updatedAt", "url"},
	"IssueCommentConnection":        {"nodes", "edges", "pageInfo", "totalCount"},
	"CreateIssuePayload":            {"issue", "clientMutationId"},
	"CloseIssuePayload":             {"issue", "clientMutationId"},
	"CreatePullRequestPayload":      {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":       {"pullRequest", "clientMutationId"},
	"AddCommentPayload":             {"commentEdge", "subject", "clientMutationId"},
	"CommentEdge":                   {"node", "cursor"},
	"__Type":                        {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "ofType", "specifiedByURL"},
	"__Field":                       {"name", "description", "args", "type", "isDeprecated", "deprecationReason"},
	"__EnumValue":                   {"name", "description", "isDeprecated", "deprecationReason"},
	"__Schema":                      {"types", "queryType", "mutationType", "subscriptionType", "directives"},
	"Node":                          {"id"},
}

// Insertion order of TYPE_FIELDS keys (JS Object.keys order), used by
// __Schema.types.
var typeFieldsOrder = []string{
	"Query", "Mutation", "Repository", "Issue", "PullRequest", "User", "Ref", "Commit", "Tree",
	"PageInfo", "IssueConnection", "IssueEdge", "PullRequestConnection", "PullRequestEdge",
	"PullRequestChangedFileConnection", "PullRequestChangedFile", "Label", "LabelConnection",
	"IssueComment", "IssueCommentConnection", "CreateIssuePayload", "CloseIssuePayload",
	"CreatePullRequestPayload", "MergePullRequestPayload", "AddCommentPayload", "CommentEdge",
	"__Type", "__Field", "__EnumValue", "__Schema", "Node",
}

var childType = map[string]map[string]string{
	"Query": {"viewer": "User", "repository": "Repository", "node": "Node", "user": "User", "organization": "User", "__type": "__Type", "__schema": "__Schema"},
	"Mutation": {
		"createIssue": "CreateIssuePayload", "closeIssue": "CloseIssuePayload", "reopenIssue": "CloseIssuePayload",
		"updateIssue": "CreateIssuePayload", "createPullRequest": "CreatePullRequestPayload",
		"mergePullRequest": "MergePullRequestPayload", "addComment": "AddCommentPayload",
		"updatePullRequest": "CreatePullRequestPayload", "closePullRequest": "MergePullRequestPayload",
		"addLabelsToLabelable": "CreateIssuePayload", "removeLabelsFromLabelable": "CreateIssuePayload",
	},
	"Repository": {
		"owner": "User", "defaultBranchRef": "Ref", "issue": "Issue", "issues": "IssueConnection",
		"pullRequest": "PullRequest", "pullRequests": "PullRequestConnection", "parent": "Repository",
	},
	"Issue":       {"author": "User", "labels": "LabelConnection", "comments": "IssueCommentConnection", "repository": "Repository"},
	"PullRequest": {"files": "PullRequestChangedFileConnection", "author": "User", "mergeCommit": "Commit", "baseRef": "Ref", "headRef": "Ref", "repository": "Repository"},
	"Ref":         {"target": "Commit", "repository": "Repository"},
	"IssueConnection":               {"nodes": "Issue", "edges": "IssueEdge", "pageInfo": "PageInfo"},
	"IssueEdge":                     {"node": "Issue"},
	"PullRequestConnection":         {"nodes": "PullRequest", "edges": "PullRequestEdge", "pageInfo": "PageInfo"},
	"PullRequestEdge":               {"node": "PullRequest"},
	"PullRequestChangedFileConnection": {"nodes": "PullRequestChangedFile", "pageInfo": "PageInfo"},
	"LabelConnection":               {"nodes": "Label", "pageInfo": "PageInfo"},
	"IssueCommentConnection":        {"nodes": "IssueComment", "pageInfo": "PageInfo"},
	"CreateIssuePayload":            {"issue": "Issue"},
	"CloseIssuePayload":             {"issue": "Issue"},
	"CreatePullRequestPayload":      {"pullRequest": "PullRequest"},
	"MergePullRequestPayload":       {"pullRequest": "PullRequest"},
	"AddCommentPayload":             {"commentEdge": "CommentEdge", "subject": "Issue"},
	"CommentEdge":                   {"node": "IssueComment"},
	"__Type":                        {"fields": "__Field", "enumValues": "__EnumValue", "possibleTypes": "__Type", "inputFields": "__Field", "ofType": "__Type"},
	"__Schema":                      {"types": "__Type", "queryType": "__Type", "mutationType": "__Type", "subscriptionType": "__Type"},
}

var searchTypeValues = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}

// -------------------------------------------------------------------- nodes
type gNode struct {
	kind        string
	repo        *Repo
	issue       *Issue
	pull        *Pull
	login       string
	name        string
	items       []*gNode
	hasNextPage bool
	endCursor   *string
	totalCount  *int
	payloadIssue *gNode
	payloadPull  *gNode
	commentNode  *gNode
	commentID    int
	body         *string
	path         string
}

func repoNode(repo *Repo) *gNode  { return &gNode{kind: "Repository", repo: repo} }
func issueNode(repo *Repo, issue *Issue) *gNode {
	return &gNode{kind: "Issue", repo: repo, issue: issue}
}
func pullNode(repo *Repo, pull *Pull) *gNode {
	return &gNode{kind: "PullRequest", repo: repo, pull: pull}
}

func encodeCursor(index int) string {
	return base64Encode([]byte("cursor:" + itoa(int64(index))))
}

func decodeCursor(cursor string) float64 {
	text := string(lenientBase64Decode(cursor))
	n, ok := parseJSNumberString(strings.Replace(text, "cursor:", "", 1))
	if !ok {
		return -1
	}
	return n
}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func issuesConnection(repo *Repo, args map[string]interface{}) *gNode {
	var list []*Issue
	for _, i := range repo.Issues {
		list = append(list, i)
	}
	if states, ok := args["states"].([]interface{}); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[strings.ToLower(jsString(s))] = true
		}
		var filtered []*Issue
		for _, i := range list {
			if wanted[i.State] {
				filtered = append(filtered, i)
			}
		}
		list = filtered
	}
	sortIssuesAsc(list)
	if orderBy, ok := args["orderBy"].(map[string]interface{}); ok {
		if jsString(orderBy["direction"]) == "DESC" {
			reverseIssues(list)
		}
	}
	first := len(list)
	if f, ok := jsNumber(args["first"]); ok {
		first = int(f)
	}
	start := 0
	if after, ok := args["after"].(string); ok {
		start = int(decodeCursor(after)) + 1
	}
	var slice []*Issue
	if start < len(list) {
		end := start + first
		if first < 0 {
			end = start
		}
		if end > len(list) {
			end = len(list)
		}
		if end < start {
			end = start
		}
		slice = list[start:end]
	}
	hasNextPage := start+len(slice) < len(list)
	var endCursor *string
	if len(slice) > 0 {
		c := encodeCursor(start + len(slice) - 1)
		endCursor = &c
	} else if after, ok := args["after"].(string); ok {
		endCursor = &after
	}
	items := make([]*gNode, len(slice))
	for i, issue := range slice {
		items[i] = issueNode(repo, issue)
	}
	total := len(list)
	return &gNode{kind: "IssueConnection", repo: repo, items: items, hasNextPage: hasNextPage, endCursor: endCursor, totalCount: &total}
}

func resolveField(store *Store, node *gNode, field string, args map[string]interface{}) interface{} {
	repo := node.repo
	switch node.kind {
	case "Query":
		switch field {
		case "viewer":
			return &gNode{kind: "User", login: store.viewerLogin()}
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			if found := store.resolveRepo(ownerStr(args), nameStr(args)); found != nil {
				_ = owner
				_ = name
				return repoNode(found)
			}
			return nil
		case "node":
			return resolveNodeByID(store, argString(args, "id"))
		case "__type":
			name := argString(args, "name")
			if _, ok := typeFields[name]; ok || name == "SearchType" || name == "String" || name == "Int" || name == "Boolean" || name == "ID" || name == "Float" {
				return &gNode{kind: "__Type", name: name}
			}
			return nil
		case "__schema":
			return &gNode{kind: "__Schema"}
		case "user":
			login := store.viewerLogin()
			if v, ok := args["login"].(string); ok {
				login = v
			}
			return &gNode{kind: "User", login: login}
		}
		return nil
	case "Mutation":
		return resolveMutation(store, field, args)
	case "User":
		switch field {
		case "login":
			return node.login
		case "id":
			return globalId("User", 1000)
		case "databaseId":
			return 1000
		case "name":
			return node.login
		case "email":
			return node.login + "@example.invalid"
		case "url", "avatarUrl":
			return ""
		}
		return nil
	case "Repository":
		return resolveRepoField(store, node, field, args)
	case "Issue":
		return resolveIssueField(store, node, field, args)
	case "PullRequest":
		return resolvePullField(store, node, field, args)
	case "Ref":
		switch field {
		case "name":
			return node.name
		case "id":
			repoID := 0
			if repo != nil {
				repoID = repo.ID
			}
			return globalId("Ref", repoID, node.name)
		case "prefix":
			return "refs/heads/"
		case "repository":
			if repo != nil {
				return repoNode(repo)
			}
			return nil
		case "target":
			if repo != nil {
				if oid, ok := repo.Refs["refs/heads/"+node.name]; ok {
					return &gNode{kind: "Commit", name: oid}
				}
			}
			return nil
		}
		return nil
	case "Commit":
		if field == "oid" || field == "id" {
			return node.name
		}
		return nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection", "LabelConnection", "IssueCommentConnection":
		switch field {
		case "nodes":
			out := make([]interface{}, len(node.items))
			for i, item := range node.items {
				out[i] = item
			}
			return out
		case "totalCount":
			if node.totalCount != nil {
				return *node.totalCount
			}
			return len(node.items)
		case "pageInfo":
			return &gNode{kind: "PageInfo", hasNextPage: node.hasNextPage, endCursor: node.endCursor}
		case "edges":
			edgeKind := strings.Replace(node.kind, "Connection", "Edge", 1)
			out := make([]interface{}, len(node.items))
			for i, item := range node.items {
				out[i] = &gNode{kind: edgeKind, payloadIssue: item, name: strOrEmpty(node.endCursor)}
			}
			return out
		}
		return nil
	case "PageInfo":
		switch field {
		case "hasNextPage":
			return node.hasNextPage
		case "hasPreviousPage":
			return false
		case "endCursor", "startCursor":
			if node.endCursor != nil {
				return *node.endCursor
			}
			return nil
		}
		return nil
	case "Label":
		switch field {
		case "name":
			return node.name
		case "color":
			if node.body != nil {
				return *node.body
			}
			return "ededed"
		case "id":
			return globalId("Label", node.name)
		}
		return nil
	case "PullRequestChangedFile":
		if field == "path" {
			return node.path
		}
		return nil
	case "IssueComment":
		switch field {
		case "id":
			return globalId("IssueComment", node.commentID)
		case "body":
			if node.body != nil {
				return *node.body
			}
			return nil
		case "author":
			return &gNode{kind: "User", login: store.viewerLogin()}
		}
		return nil
	case "CreateIssuePayload", "CloseIssuePayload":
		switch field {
		case "issue":
			if node.payloadIssue != nil {
				return node.payloadIssue
			}
			return nil
		case "clientMutationId":
			return nil
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		switch field {
		case "pullRequest":
			if node.payloadPull != nil {
				return node.payloadPull
			}
			return nil
		case "clientMutationId":
			return nil
		}
		return nil
	case "AddCommentPayload":
		switch field {
		case "commentEdge":
			return &gNode{kind: "CommentEdge", commentNode: node.commentNode}
		case "subject":
			if node.payloadIssue != nil {
				return node.payloadIssue
			}
			return nil
		case "clientMutationId":
			return nil
		}
		return nil
	case "CommentEdge":
		switch field {
		case "node":
			if node.commentNode != nil {
				return node.commentNode
			}
			return nil
		case "cursor":
			return encodeCursor(0)
		}
		return nil
	case "__Type":
		switch field {
		case "name":
			return node.name
		case "kind":
			if node.name == "SearchType" {
				return "ENUM"
			}
			if _, ok := typeFields[node.name]; ok {
				return "OBJECT"
			}
			return "SCALAR"
		case "fields":
			fields, ok := typeFields[node.name]
			if !ok {
				return nil
			}
			out := make([]interface{}, len(fields))
			for i, name := range fields {
				out[i] = &gNode{kind: "__Field", name: name}
			}
			return out
		case "enumValues":
			if node.name != "SearchType" {
				return nil
			}
			out := make([]interface{}, len(searchTypeValues))
			for i, name := range searchTypeValues {
				out[i] = &gNode{kind: "__EnumValue", name: name}
			}
			return out
		}
		return nil
	case "__Field":
		switch field {
		case "name":
			return node.name
		case "description":
			return nil
		case "isDeprecated":
			return false
		}
		return nil
	case "__EnumValue":
		switch field {
		case "name":
			return node.name
		case "description":
			return nil
		case "isDeprecated":
			return false
		}
		return nil
	case "__Schema":
		switch field {
		case "queryType":
			return &gNode{kind: "__Type", name: "Query"}
		case "mutationType":
			return &gNode{kind: "__Type", name: "Mutation"}
		case "types":
			out := make([]interface{}, len(typeFieldsOrder))
			for i, name := range typeFieldsOrder {
				out[i] = &gNode{kind: "__Type", name: name}
			}
			return out
		}
		return nil
	}
	return nil
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// argString mirrors JS String(args.x ?? '').
func argString(args map[string]interface{}, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	return jsString(v)
}

func ownerStr(args map[string]interface{}) string { return argString(args, "owner") }
func nameStr(args map[string]interface{}) string  { return argString(args, "name") }

func resolveRepoField(store *Store, node *gNode, field string, args map[string]interface{}) interface{} {
	repo := node.repo
	switch field {
	case "id":
		return globalId("Repository", repo.ID)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return store.viewerLogin() + "/" + repo.Name
	case "owner":
		return &gNode{kind: "User", login: store.viewerLogin()}
	case "defaultBranchRef":
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			return &gNode{kind: "Ref", repo: repo, name: repo.DefaultBranch}
		}
		return nil
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "description":
		if repo.Description != nil {
			return *repo.Description
		}
		return nil
	case "isPrivate":
		return repo.IsPrivate
	case "issue":
		if n, ok := jsNumber(args["number"]); ok {
			if issue, ok := repo.Issues[int(n)]; ok {
				return issueNode(repo, issue)
			}
		}
		return nil
	case "issues":
		return issuesConnection(repo, args)
	case "pullRequest":
		if n, ok := jsNumber(args["number"]); ok {
			if pull, ok := repo.Pulls[int(n)]; ok {
				return pullNode(repo, pull)
			}
		}
		return nil
	case "pullRequests":
		var list []*Pull
		for _, p := range repo.Pulls {
			list = append(list, p)
		}
		sortPullsAsc(list)
		if states, ok := args["states"].([]interface{}); ok && len(states) > 0 {
			wanted := map[string]bool{}
			for _, s := range states {
				wanted[jsString(s)] = true
			}
			var filtered []*Pull
			for _, p := range list {
				state := strings.ToUpper(p.State)
				if p.Merged {
					state = "MERGED"
				}
				if wanted[state] {
					filtered = append(filtered, p)
				}
			}
			list = filtered
		}
		first := len(list)
		if f, ok := jsNumber(args["first"]); ok {
			first = int(f)
		}
		if first < 0 {
			first = 0
		}
		slice := list
		if first < len(list) {
			slice = list[:first]
		}
		items := make([]*gNode, len(slice))
		for i, p := range slice {
			items[i] = pullNode(repo, p)
		}
		total := len(list)
		return &gNode{kind: "PullRequestConnection", repo: repo, items: items, totalCount: &total, hasNextPage: false, endCursor: nil}
	case "createdAt":
		return repo.CreatedAt
	case "updatedAt":
		return repo.UpdatedAt
	case "stargazerCount", "forkCount":
		return 0
	case "url":
		return ""
	}
	return nil
}

func resolveIssueField(store *Store, node *gNode, field string, args map[string]interface{}) interface{} {
	repo := node.repo
	issue := node.issue
	switch field {
	case "id":
		return globalId("Issue", repo.ID, issue.Number)
	case "number":
		return issue.Number
	case "title":
		return issue.Title
	case "body":
		return issue.Body
	case "state":
		return strings.ToUpper(issue.State)
	case "author":
		return &gNode{kind: "User", login: store.viewerLogin()}
	case "repository":
		return repoNode(repo)
	case "createdAt":
		return issue.CreatedAt
	case "updatedAt":
		return issue.UpdatedAt
	case "closed":
		return issue.State == "closed"
	case "url":
		return ""
	case "labels":
		items := make([]*gNode, len(issue.Labels))
		for i, name := range issue.Labels {
			var color *string
			if l, ok := repo.Labels[name]; ok {
				c := l.Color
				color = &c
			}
			items[i] = &gNode{kind: "Label", name: name, body: color}
		}
		total := len(issue.Labels)
		return &gNode{kind: "LabelConnection", items: items, totalCount: &total}
	case "comments":
		list := repo.Comments[issue.Number]
		items := make([]*gNode, len(list))
		for i, c := range list {
			b := c.Body
			items[i] = &gNode{kind: "IssueComment", repo: repo, commentID: c.ID, body: &b}
		}
		total := len(list)
		return &gNode{kind: "IssueCommentConnection", items: items, totalCount: &total}
	}
	return nil
}

func resolvePullField(store *Store, node *gNode, field string, args map[string]interface{}) interface{} {
	repo := node.repo
	pull := node.pull
	switch field {
	case "id":
		return globalId("PullRequest", repo.ID, pull.Number)
	case "number":
		return pull.Number
	case "title":
		return pull.Title
	case "body":
		return pull.Body
	case "state":
		if pull.Merged {
			return "MERGED"
		}
		return strings.ToUpper(pull.State)
	case "merged":
		return pull.Merged
	case "headRefName":
		return pull.Head
	case "baseRefName":
		return pull.Base
	case "mergeable":
		if pull.Merged {
			return "UNKNOWN"
		}
		computed := computeMerge(repo, pull)
		if computed == nil {
			return "UNKNOWN"
		}
		if computed.Conflict {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	case "files":
		changes := pullChanges(repo, pull)
		first := len(changes)
		if f, ok := jsNumber(args["first"]); ok {
			first = int(f)
		}
		if first < 0 {
			first = 0
		}
		slice := changes
		if first < len(changes) {
			slice = changes[:first]
		}
		items := make([]*gNode, len(slice))
		for i, c := range slice {
			items[i] = &gNode{kind: "PullRequestChangedFile", path: c.Filename}
		}
		total := len(changes)
		return &gNode{kind: "PullRequestChangedFileConnection", items: items, totalCount: &total}
	case "author":
		return &gNode{kind: "User", login: store.viewerLogin()}
	case "repository":
		return repoNode(repo)
	case "createdAt":
		return pull.CreatedAt
	case "updatedAt":
		return pull.UpdatedAt
	case "closed":
		return pull.State == "closed"
	case "mergedAt":
		if pull.MergedAt != nil {
			return *pull.MergedAt
		}
		return nil
	case "additions", "deletions", "changedFiles":
		changes := pullChanges(repo, pull)
		if field == "changedFiles" {
			return len(changes)
		}
		sum := 0
		for _, c := range changes {
			if field == "additions" {
				sum += c.Additions
			} else {
				sum += c.Deletions
			}
		}
		return sum
	case "url":
		return ""
	}
	return nil
}

func resolveNodeByID(store *Store, id string) *gNode {
	decoded := decodeGlobalId(id)
	if decoded == nil {
		return nil
	}
	switch decoded.Type {
	case "Repository":
		if len(decoded.Parts) >= 1 {
			if n, ok := parseJSNumberString(decoded.Parts[0]); ok {
				if repo := store.repoByID(int(n)); repo != nil {
					return repoNode(repo)
				}
			}
		}
	case "Issue":
		if len(decoded.Parts) >= 2 {
			rn, ok1 := parseJSNumberString(decoded.Parts[0])
			in, ok2 := parseJSNumberString(decoded.Parts[1])
			if ok1 && ok2 {
				if repo := store.repoByID(int(rn)); repo != nil {
					if issue, ok := repo.Issues[int(in)]; ok {
						return issueNode(repo, issue)
					}
				}
			}
		}
	case "PullRequest":
		if len(decoded.Parts) >= 2 {
			rn, ok1 := parseJSNumberString(decoded.Parts[0])
			pn, ok2 := parseJSNumberString(decoded.Parts[1])
			if ok1 && ok2 {
				if repo := store.repoByID(int(rn)); repo != nil {
					if pull, ok := repo.Pulls[int(pn)]; ok {
						return pullNode(repo, pull)
					}
				}
			}
		}
	case "User":
		return &gNode{kind: "User", login: store.viewerLogin()}
	}
	return nil
}

// inputRepo / inputIssue / inputPull decode a global id for mutations.
func mutationRepo(store *Store, input map[string]interface{}, key, wantType string) *Repo {
	decoded := decodeGlobalId(argString(input, key))
	if decoded == nil || decoded.Type != wantType || len(decoded.Parts) < 1 {
		return nil
	}
	n, ok := parseJSNumberString(decoded.Parts[0])
	if !ok {
		return nil
	}
	return store.repoByID(int(n))
}

func resolveMutation(store *Store, field string, args map[string]interface{}) interface{} {
	input, _ := args["input"].(map[string]interface{})
	if input == nil {
		input = map[string]interface{}{}
	}
	switch field {
	case "createIssue":
		repo := mutationRepo(store, input, "repositoryId", "Repository")
		if repo == nil {
			panic(&gqlError{"Repository not found"})
		}
		title := argString(input, "title")
		if title == "" {
			panic(&gqlError{"Title is required"})
		}
		issue := createIssue(store, repo, title, argString(input, "body"), nil)
		return &gNode{kind: "CreateIssuePayload", payloadIssue: issueNode(repo, issue)}
	case "closeIssue", "reopenIssue":
		decoded := decodeGlobalId(argString(input, "issueId"))
		var repo *Repo
		var issue *Issue
		if decoded != nil && decoded.Type == "Issue" && len(decoded.Parts) >= 2 {
			if n, ok := parseJSNumberString(decoded.Parts[0]); ok {
				repo = store.repoByID(int(n))
			}
			if repo != nil {
				if num, ok := parseJSNumberString(decoded.Parts[1]); ok {
					issue = repo.Issues[int(num)]
				}
			}
		}
		if repo == nil || issue == nil {
			panic(&gqlError{"Issue not found"})
		}
		if field == "closeIssue" {
			issue.State = "closed"
		} else {
			issue.State = "open"
		}
		issue.UpdatedAt = store.nowIso()
		return &gNode{kind: "CloseIssuePayload", payloadIssue: issueNode(repo, issue)}
	case "createPullRequest":
		repo := mutationRepo(store, input, "repositoryId", "Repository")
		if repo == nil {
			panic(&gqlError{"Repository not found"})
		}
		head := argString(input, "headRefName")
		base := argString(input, "baseRefName")
		if _, ok := repo.Refs["refs/heads/"+head]; !ok {
			panic(&gqlError{"Head branch \"" + head + "\" does not exist"})
		}
		if _, ok := repo.Refs["refs/heads/"+base]; !ok {
			panic(&gqlError{"Base branch \"" + base + "\" does not exist"})
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
			pull = createPull(store, repo, argString(input, "title"), argString(input, "body"), head, base)
		}
		return &gNode{kind: "CreatePullRequestPayload", payloadPull: pullNode(repo, pull)}
	case "mergePullRequest":
		decoded := decodeGlobalId(argString(input, "pullRequestId"))
		var repo *Repo
		var pull *Pull
		if decoded != nil && decoded.Type == "PullRequest" && len(decoded.Parts) >= 2 {
			if n, ok := parseJSNumberString(decoded.Parts[0]); ok {
				repo = store.repoByID(int(n))
			}
			if repo != nil {
				if num, ok := parseJSNumberString(decoded.Parts[1]); ok {
					pull = repo.Pulls[int(num)]
				}
			}
		}
		if repo == nil || pull == nil {
			panic(&gqlError{"Pull request not found"})
		}
		if !pull.Merged {
			method := "squash"
			if strings.ToUpper(argString(input, "mergeMethod")) == "MERGE" {
				method = "merge"
			}
			_, outcome := mergePull(store, repo, pull, method)
			if outcome == "conflict" || outcome == "null" {
				panic(&gqlError{"Pull request is not mergeable"})
			}
		}
		return &gNode{kind: "MergePullRequestPayload", payloadPull: pullNode(repo, pull)}
	case "addComment":
		decoded := decodeGlobalId(argString(input, "subjectId"))
		var repo *Repo
		var issue *Issue
		if decoded != nil && decoded.Type == "Issue" && len(decoded.Parts) >= 2 {
			if n, ok := parseJSNumberString(decoded.Parts[0]); ok {
				repo = store.repoByID(int(n))
			}
			if repo != nil {
				if num, ok := parseJSNumberString(decoded.Parts[1]); ok {
					issue = repo.Issues[int(num)]
				}
			}
		}
		if repo == nil || issue == nil {
			panic(&gqlError{"Subject not found"})
		}
		comment := &Comment{ID: store.NextComment, Body: argString(input, "body")}
		store.NextComment++
		repo.Comments[issue.Number] = append(repo.Comments[issue.Number], comment)
		b := comment.Body
		return &gNode{
			kind:         "AddCommentPayload",
			commentNode:  &gNode{kind: "IssueComment", repo: repo, commentID: comment.ID, body: &b},
			payloadIssue: issueNode(repo, issue),
		}
	}
	panic(&gqlError{"Mutation " + field + " is not supported"})
}

// ---------------------------------------------------------------- execution
type execCtx struct {
	store     *Store
	vars      map[string]interface{}
	defaults  map[string]*gValue
	fragments map[string]*fragmentDef
}

func expandSelections(selections []gSel, nodeKind string, ctx *execCtx, depth int) []*fieldSel {
	if depth > 16 {
		return nil
	}
	var out []*fieldSel
	for _, sel := range selections {
		switch sel.kind {
		case "field":
			out = append(out, sel.field)
		case "inline":
			if sel.typeCondition == "" || sel.typeCondition == nodeKind || sel.typeCondition == "Node" {
				out = append(out, expandSelections(sel.selectionSet, nodeKind, ctx, depth+1)...)
			}
		case "spread":
			if frag, ok := ctx.fragments[sel.name]; ok {
				if frag.typeCondition == "" || frag.typeCondition == nodeKind || frag.typeCondition == "Node" {
					out = append(out, expandSelections(frag.selectionSet, nodeKind, ctx, depth+1)...)
				}
			}
		}
	}
	return out
}

func validateSelections(selections []gSel, typeName string, ctx *execCtx, errors *[]string, depth int) {
	if depth > 24 {
		return
	}
	fieldSet := map[string]bool{}
	for _, f := range typeFields[typeName] {
		fieldSet[f] = true
	}
	if typeName == "Node" {
		for _, f := range typeFields["Repository"] {
			fieldSet[f] = true
		}
	}
	for _, sel := range selections {
		switch sel.kind {
		case "inline":
			cond := sel.typeCondition
			if cond == "" {
				cond = typeName
			}
			validateSelections(sel.selectionSet, cond, ctx, errors, depth+1)
		case "spread":
			if frag, ok := ctx.fragments[sel.name]; ok {
				validateSelections(frag.selectionSet, frag.typeCondition, ctx, errors, depth+1)
			}
		case "field":
			if sel.field.name == "__typename" {
				continue
			}
			if !fieldSet[sel.field.name] {
				*errors = append(*errors, "Field '"+sel.field.name+"' doesn't exist on type '"+typeName+"'")
				continue
			}
			if sel.field.selectionSet != nil {
				if child, ok := childType[typeName][sel.field.name]; ok {
					validateSelections(sel.field.selectionSet, child, ctx, errors, depth+1)
				}
			}
		}
	}
}

func completeValue(ctx *execCtx, node *gNode, selections []gSel) *OMap {
	out := newOMap()
	fields := expandSelections(selections, node.kind, ctx, 0)
	for _, field := range fields {
		key := field.name
		if field.hasAlias {
			key = field.alias
		}
		if field.name == "__typename" {
			out.set(key, node.kind)
			continue
		}
		args := map[string]interface{}{}
		for _, argName := range field.argOrder {
			args[argName] = evalValue(field.args[argName], ctx.vars, ctx.defaults)
		}
		value := resolveField(ctx.store, node, field.name, args)
		out.set(key, completeAny(ctx, value, field.selectionSet))
	}
	return out
}

func completeAny(ctx *execCtx, value interface{}, selectionSet []gSel) interface{} {
	if value == nil {
		return nil
	}
	if list, ok := value.([]interface{}); ok {
		out := make([]interface{}, len(list))
		for i, item := range list {
			out[i] = completeAny(ctx, item, selectionSet)
		}
		return out
	}
	if node, ok := value.(*gNode); ok {
		if selectionSet != nil {
			return completeValue(ctx, node, selectionSet)
		}
		return node
	}
	return value
}

func executeOperation(store *Store, op *gOperation, vars map[string]interface{}, fragments map[string]*fragmentDef) (result *OMap) {
	ctx := &execCtx{store: store, vars: vars, defaults: op.varDefaults, fragments: fragments}
	rootKind := "Query"
	if op.typ == "mutation" {
		rootKind = "Mutation"
	}
	var errs []string
	validateSelections(op.selectionSet, rootKind, ctx, &errs, 0)
	if len(errs) > 0 {
		out := newOMap()
		out.set("data", nil)
		errList := make([]interface{}, len(errs))
		for i, msg := range errs {
			m := newOMap()
			m.set("message", msg)
			errList[i] = m
		}
		out.set("errors", errList)
		return out
	}
	defer func() {
		if r := recover(); r != nil {
			if ge, ok := r.(*gqlError); ok {
				out := newOMap()
				out.set("data", nil)
				m := newOMap()
				m.set("message", ge.msg)
				out.set("errors", []interface{}{m})
				result = out
				return
			}
			panic(r)
		}
	}()
	root := &gNode{kind: rootKind}
	data := completeValue(ctx, root, op.selectionSet)
	out := newOMap()
	out.set("data", data)
	return out
}

func handleGraphql(ctx *Ctx) Reply {
	store := ctx.Store
	if ctx.Method != "POST" {
		return errorReply(404, "Not Found")
	}
	if ctx.JSONError {
		return jsonReply(400, errorOnlyOMap("Invalid JSON body"), nil)
	}
	isBatch := false
	var items []interface{}
	if batch, ok := ctx.JSON.([]interface{}); ok {
		isBatch = true
		items = batch
	} else {
		items = []interface{}{ctx.JSON}
	}
	var results []interface{}
	for _, item := range items {
		results = append(results, executeGraphqlItem(store, item))
	}
	if isBatch {
		return jsonReply(200, results, nil)
	}
	return jsonReply(200, results[0], nil)
}

func errorOnlyOMap(message string) *OMap {
	out := newOMap()
	m := newOMap()
	m.set("message", message)
	out.set("errors", []interface{}{m})
	return out
}

func executeGraphqlItem(store *Store, item interface{}) interface{} {
	obj, _ := item.(map[string]interface{})
	queryVal, _ := obj["query"].(string)
	if obj == nil || queryVal == "" && obj["query"] == nil {
		// Mirror: typeof query !== 'string' -> error entry.
		if _, isStr := obj["query"].(string); !isStr {
			return errorOnlyOMap("Must provide a query string")
		}
	}
	query, isStr := obj["query"].(string)
	if !isStr {
		return errorOnlyOMap("Must provide a query string")
	}
	vars, _ := obj["variables"].(map[string]interface{})
	if vars == nil {
		vars = map[string]interface{}{}
	}
	tokens, err := tokenizeGraphql(query)
	if err != nil {
		return errorOnlyOMap(err.Error())
	}
	parser := &gParser{tokens: tokens}
	operations, fragments, err := parser.parseDocument()
	if err != nil {
		return errorOnlyOMap(err.Error())
	}
	if len(operations) == 0 {
		return errorOnlyOMap("No operation found")
	}
	return executeOperation(store, operations[0], vars, fragments)
}

var _ = sort.Strings
