package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// REST v3 surface (ported from rest.ts).

func repoJSON(store *Store, repo *Repo) map[string]interface{} {
	owner := store.viewerLogin()
	openIssues := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			openIssues++
		}
	}
	var desc interface{}
	if repo.Description != nil {
		desc = *repo.Description
	}
	return map[string]interface{}{
		"id":                repo.ID,
		"node_id":           "R_" + itoa(int64(repo.ID)),
		"name":              repo.Name,
		"full_name":         owner + "/" + repo.Name,
		"owner":             map[string]interface{}{"login": owner, "id": 1000, "type": "User"},
		"private":           repo.IsPrivate,
		"description":       desc,
		"default_branch":    repo.DefaultBranch,
		"html_url":          "http://forge.local/" + owner + "/" + repo.Name,
		"clone_url":         "http://forge.local/" + owner + "/" + repo.Name + ".git",
		"has_issues":        true,
		"open_issues_count": openIssues,
		"created_at":        repo.CreatedAt,
		"updated_at":        repo.UpdatedAt,
	}
}

func issueJSON(store *Store, repo *Repo, issue *Issue) map[string]interface{} {
	labels := make([]interface{}, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, map[string]interface{}{"id": 1, "name": name, "color": color})
	}
	return map[string]interface{}{
		"id":         issue.ID,
		"node_id":    "I_" + itoa(int64(issue.ID)),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]interface{}{"login": store.viewerLogin(), "id": 1000},
		"comments":   len(repo.Comments[issue.Number]),
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
	}
}

func pullJSON(store *Store, repo *Repo, pull *Pull) map[string]interface{} {
	headSha, ok := repo.Refs["refs/heads/"+pull.Head]
	if !ok {
		headSha = ZeroOID
	}
	baseSha, ok := repo.Refs["refs/heads/"+pull.Base]
	if !ok {
		baseSha = ZeroOID
	}
	owner := store.viewerLogin()
	var mergedAt interface{}
	if pull.MergedAt != nil {
		mergedAt = *pull.MergedAt
	}
	var mergeSha interface{}
	if pull.MergeCommitSha != nil {
		mergeSha = *pull.MergeCommitSha
	}
	return map[string]interface{}{
		"id":               pull.ID,
		"node_id":          "PR_" + itoa(int64(pull.ID)),
		"number":           pull.Number,
		"state":            pull.State,
		"title":            pull.Title,
		"body":             pull.Body,
		"head":             map[string]interface{}{"ref": pull.Head, "sha": headSha, "label": owner + ":" + pull.Head},
		"base":             map[string]interface{}{"ref": pull.Base, "sha": baseSha, "label": owner + ":" + pull.Base},
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSha,
		"user":             map[string]interface{}{"login": owner, "id": 1000},
		"created_at":       pull.CreatedAt,
		"updated_at":       pull.UpdatedAt,
	}
}

// issueJSONBytes is a hand-written fast serializer for the read/write
// bench hot paths; it produces the same JSON as issueJSON.
func issueJSONBytes(store *Store, repo *Repo, issue *Issue) []byte {
	buf := make([]byte, 0, 320)
	buf = append(buf, `{"id":`...)
	buf = append(buf, itoa(int64(issue.ID))...)
	buf = append(buf, `,"node_id":"I_`...)
	buf = append(buf, itoa(int64(issue.ID))...)
	buf = append(buf, `","number":`...)
	buf = append(buf, itoa(int64(issue.Number))...)
	buf = append(buf, `,"title":`...)
	buf = appendJSONString(buf, issue.Title)
	buf = append(buf, `,"body":`...)
	buf = appendJSONString(buf, issue.Body)
	buf = append(buf, `,"state":`...)
	buf = appendJSONString(buf, issue.State)
	buf = append(buf, `,"labels":[`...)
	for i, name := range issue.Labels {
		if i > 0 {
			buf = append(buf, ',')
		}
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		buf = append(buf, `{"id":1,"name":`...)
		buf = appendJSONString(buf, name)
		buf = append(buf, `,"color":`...)
		buf = appendJSONString(buf, color)
		buf = append(buf, '}')
	}
	buf = append(buf, `],"user":{"login":`...)
	buf = appendJSONString(buf, store.viewerLogin())
	buf = append(buf, `,"id":1000},"comments":`...)
	buf = append(buf, itoa(int64(len(repo.Comments[issue.Number])))...)
	buf = append(buf, `,"created_at":`...)
	buf = appendJSONString(buf, issue.CreatedAt)
	buf = append(buf, `,"updated_at":`...)
	buf = appendJSONString(buf, issue.UpdatedAt)
	buf = append(buf, '}')
	return buf
}

func issueReply(status int, store *Store, repo *Repo, issue *Issue, headers map[string]string) Reply {
	if headers == nil {
		headers = map[string]string{}
	}
	return Reply{Status: status, Headers: headers, JSON: json.RawMessage(issueJSONBytes(store, repo, issue)), HasJSON: true}
}

func createIssue(store *Store, repo *Repo, title, body string, labels []string) *Issue {
	ts := store.nowIso()
	issue := &Issue{
		ID:        store.NextIssueID,
		Number:    repo.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string{}, labels...),
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	store.NextIssueID++
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	return issue
}

func createPull(store *Store, repo *Repo, title, body, head, base string) *Pull {
	ts := store.nowIso()
	pull := &Pull{
		ID:        store.NextPullID,
		Number:    repo.NextNumber,
		Title:     title,
		Body:      body,
		Head:      head,
		Base:      base,
		State:     "open",
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	store.NextPullID++
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	return pull
}

func stripOwnerPrefix(ref string) string {
	if idx := strings.Index(ref, ":"); idx != -1 {
		return ref[idx+1:]
	}
	return ref
}

// ------------------------------------------------------------------ handler
func handleRest(ctx *Ctx) Reply {
	store := ctx.Store
	seg := ctx.Segments[2:]

	if (ctx.Method == "POST" || ctx.Method == "PUT" || ctx.Method == "PATCH" || ctx.Method == "DELETE") && ctx.JSONError {
		return errorReply(400, "Problems parsing JSON")
	}

	if len(seg) == 0 {
		if ctx.Method != "GET" {
			return errorReply(404, "Not Found")
		}
		return jsonReply(200, map[string]interface{}{
			"current_user_url":  "/api/v3/user",
			"repository_url":    "/api/v3/repos/{owner}/{repo}",
			"issue_search_url":  "/api/v3/search/issues",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}
	if len(seg) == 1 && seg[0] == "meta" {
		if ctx.Method != "GET" {
			return errorReply(404, "Not Found")
		}
		return jsonReply(200, map[string]interface{}{
			"installed_version":                    "1.0.0-forge",
			"verifiable_password_authentication":   false,
		}, nil)
	}
	if len(seg) == 1 && seg[0] == "user" {
		if ctx.Method != "GET" {
			return errorReply(404, "Not Found")
		}
		login := store.viewerLogin()
		return jsonReply(200, map[string]interface{}{
			"login": login, "id": 1000, "name": login,
			"email": login + "@example.invalid", "type": "User",
		}, nil)
	}
	if len(seg) == 2 && seg[0] == "user" && seg[1] == "repos" {
		if ctx.Method == "POST" {
			body := ctx.bodyObj()
			name, ok := strField(body["name"])
			if !ok {
				return errorReply(422, "Validation Failed: name is required")
			}
			if _, exists := store.ReposByName[name]; exists {
				return errorReply(422, "Validation Failed: name already exists on this account")
			}
			var desc *string
			if d, ok := strField(body["description"]); ok {
				desc = &d
			}
			repo := store.createRepo(name, body["private"] == true, desc)
			return jsonReply(201, repoJSON(store, repo), nil)
		}
		if ctx.Method == "GET" {
			page, link := paginate(ctx, "/api/v3/user/repos", store.Repos)
			items := make([]interface{}, 0, len(page))
			for _, r := range page {
				items = append(items, repoJSON(store, r))
			}
			return jsonReply(200, items, linkHeaders(link))
		}
		return errorReply(404, "Not Found")
	}
	if len(seg) == 3 && seg[0] == "users" && seg[2] == "repos" && ctx.Method == "GET" {
		if seg[1] != store.viewerLogin() {
			return errorReply(404, "Not Found")
		}
		page, link := paginate(ctx, "/api/v3/users/"+seg[1]+"/repos", store.Repos)
		items := make([]interface{}, 0, len(page))
		for _, r := range page {
			items = append(items, repoJSON(store, r))
		}
		return jsonReply(200, items, linkHeaders(link))
	}
	if len(seg) >= 3 && seg[0] == "repos" {
		owner := seg[1]
		name := seg[2]
		store.learnViewer(owner)
		repo := store.resolveRepo(owner, name)
		if repo == nil {
			return errorReply(404, "Not Found")
		}
		return handleRepo(ctx, repo, seg[3:])
	}
	return errorReply(404, "Not Found")
}

func linkHeaders(link string) map[string]string {
	if link == "" {
		return map[string]string{}
	}
	return map[string]string{"link": link}
}

func handleRepo(ctx *Ctx, repo *Repo, seg []string) Reply {
	store := ctx.Store
	basePath := "/api/v3/repos/" + store.viewerLogin() + "/" + repo.Name

	if len(seg) == 0 {
		if ctx.Method == "GET" {
			return jsonReply(200, repoJSON(store, repo), nil)
		}
		if ctx.Method == "PATCH" {
			body := ctx.bodyObj()
			if v, present := body["description"]; present {
				if v == nil {
					repo.Description = nil
				} else if s, ok := v.(string); ok {
					repo.Description = &s
				}
			}
			if b, ok := body["private"].(bool); ok {
				repo.IsPrivate = b
			}
			if s, ok := body["name"].(string); ok && s != repo.Name {
				delete(store.ReposByName, repo.Name)
				repo.Name = s
				store.ReposByName[repo.Name] = repo
			}
			repo.UpdatedAt = store.nowIso()
			return jsonReply(200, repoJSON(store, repo), nil)
		}
		if ctx.Method == "DELETE" {
			store.deleteRepo(repo)
			return Reply{Status: 204, Headers: map[string]string{}}
		}
		return errorReply(404, "Not Found")
	}

	if seg[0] == "branches" {
		if len(seg) == 1 && ctx.Method == "GET" {
			var branches []interface{}
			for _, ref := range repo.RefOrder {
				if !strings.HasPrefix(ref, "refs/heads/") {
					continue
				}
				branches = append(branches, map[string]interface{}{
					"name":      ref[len("refs/heads/"):],
					"commit":    map[string]interface{}{"sha": repo.Refs[ref]},
					"protected": false,
				})
			}
			if branches == nil {
				branches = []interface{}{}
			}
			return jsonReply(200, branches, nil)
		}
		if len(seg) >= 2 && ctx.Method == "GET" {
			branch := strings.Join(seg[1:], "/")
			oid, ok := repo.Refs["refs/heads/"+branch]
			if !ok {
				return errorReply(404, "Branch not found")
			}
			return jsonReply(200, map[string]interface{}{
				"name":      branch,
				"commit":    map[string]interface{}{"sha": oid},
				"protected": false,
			}, nil)
		}
		return errorReply(404, "Not Found")
	}

	if seg[0] == "commits" {
		if len(seg) == 1 && ctx.Method == "GET" {
			var ref string
			hasRef := false
			if vals, ok := ctx.Query["sha"]; ok && len(vals) > 0 {
				ref, hasRef = vals[0], true
			}
			tip := resolveCommitish(repo, ref, hasRef)
			if tip == "" {
				return jsonReply(200, []interface{}{}, nil)
			}
			page, _ := paginate(ctx, basePath+"/commits", commitHistory(repo, tip, 500))
			items := make([]interface{}, 0, len(page))
			for _, oid := range page {
				var commit *CommitData
				if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjCommit {
					c := parseCommit(obj.Data)
					commit = &c
				}
				items = append(items, commitListItem(repo, oid, commit))
			}
			return jsonReply(200, items, nil)
		}
		if len(seg) == 2 && ctx.Method == "GET" {
			oid := seg[1]
			obj, ok := repo.Objects[oid]
			if !ok || obj.Type != ObjCommit {
				return errorReply(404, "Not Found")
			}
			commit := parseCommit(obj.Data)
			return jsonReply(200, commitListItem(repo, oid, &commit), nil)
		}
		return errorReply(404, "Not Found")
	}

	if seg[0] == "git" && len(seg) >= 2 && seg[1] == "refs" {
		if len(seg) == 2 && ctx.Method == "POST" {
			body := ctx.bodyObj()
			ref, okRef := strField(body["ref"])
			sha, okSha := strField(body["sha"])
			if !okRef || !okSha {
				return errorReply(422, "Validation Failed: ref and sha are required")
			}
			obj, ok := repo.Objects[sha]
			if !ok {
				return errorReply(422, "Validation Failed: sha does not exist")
			}
			if _, exists := repo.Refs[ref]; exists {
				return errorReply(422, "Reference already exists")
			}
			repo.setRef(ref, sha)
			repo.Version++
			return jsonReply(201, map[string]interface{}{
				"ref":    ref,
				"object": map[string]interface{}{"sha": sha, "type": string(obj.Type)},
			}, nil)
		}
		if len(seg) >= 3 && ctx.Method == "GET" {
			ref := "refs/" + strings.Join(seg[2:], "/")
			oid, ok := repo.Refs[ref]
			if !ok {
				return errorReply(404, "Reference not found")
			}
			typ := "commit"
			if obj, ok := repo.Objects[oid]; ok {
				typ = string(obj.Type)
			}
			return jsonReply(200, map[string]interface{}{
				"ref":    ref,
				"object": map[string]interface{}{"sha": oid, "type": typ},
			}, nil)
		}
		if len(seg) == 2 && ctx.Method == "GET" {
			var list []interface{}
			for _, ref := range repo.RefOrder {
				oid := repo.Refs[ref]
				typ := "commit"
				if obj, ok := repo.Objects[oid]; ok {
					typ = string(obj.Type)
				}
				list = append(list, map[string]interface{}{
					"ref":    ref,
					"object": map[string]interface{}{"sha": oid, "type": typ},
				})
			}
			if list == nil {
				list = []interface{}{}
			}
			return jsonReply(200, list, nil)
		}
		return errorReply(404, "Not Found")
	}

	if seg[0] == "contents" {
		path := strings.Join(seg[1:], "/")
		return handleContents(ctx, repo, path)
	}

	if seg[0] == "labels" {
		if len(seg) == 1 && ctx.Method == "POST" {
			body := ctx.bodyObj()
			name, ok := strField(body["name"])
			if !ok {
				return errorReply(422, "Validation Failed: name is required")
			}
			color := "ededed"
			if c, ok := strField(body["color"]); ok {
				color = c
			}
			color = strings.TrimPrefix(color, "#")
			if _, exists := repo.Labels[name]; !exists {
				repo.setLabel(&Label{Name: name, Color: color})
			}
			label := repo.Labels[name]
			return jsonReply(201, map[string]interface{}{"id": 1, "name": label.Name, "color": label.Color}, nil)
		}
		if len(seg) == 1 && ctx.Method == "GET" {
			var list []interface{}
			for _, name := range repo.LabelOrder {
				l := repo.Labels[name]
				list = append(list, map[string]interface{}{"id": 1, "name": l.Name, "color": l.Color})
			}
			if list == nil {
				list = []interface{}{}
			}
			return jsonReply(200, list, nil)
		}
		return errorReply(404, "Not Found")
	}

	if seg[0] == "issues" {
		return handleIssues(ctx, repo, seg[1:], basePath)
	}
	if seg[0] == "pulls" {
		return handlePulls(ctx, repo, seg[1:], basePath)
	}
	return errorReply(404, "Not Found")
}

func commitListItem(repo *Repo, oid string, commit *CommitData) map[string]interface{} {
	message, authorName, authorEmail, committerName, committerEmail, tree := "", "", "", "", "", ZeroOID
	if commit != nil {
		message = commit.Message
		authorName, authorEmail = commit.AuthorName, commit.AuthorEmail
		committerName, committerEmail = commit.CommitterName, commit.CommitterEmail
		tree = commit.Tree
	}
	var parents []interface{}
	if commit != nil {
		for _, p := range commit.Parents {
			parents = append(parents, map[string]interface{}{"sha": p})
		}
	}
	if parents == nil {
		parents = []interface{}{}
	}
	return map[string]interface{}{
		"sha": oid,
		"commit": map[string]interface{}{
			"message":   message,
			"author":    map[string]interface{}{"name": authorName, "email": authorEmail, "date": repo.CreatedAt},
			"committer": map[string]interface{}{"name": committerName, "email": committerEmail, "date": repo.CreatedAt},
			"tree":      map[string]interface{}{"sha": tree},
		},
		"parents": parents,
	}
}

func handleContents(ctx *Ctx, repo *Repo, path string) Reply {
	store := ctx.Store
	body := ctx.bodyObj()

	if ctx.Method == "PUT" {
		contentB64, hasContent := strField(body["content"])
		message := "Update file"
		if m, ok := strField(body["message"]); ok {
			message = m
		}
		branch := repo.DefaultBranch
		if b, ok := strField(body["branch"]); ok {
			branch = b
		}
		if !hasContent {
			return errorReply(422, "Validation Failed: content is required")
		}
		cleaned := strings.Join(strings.Fields(contentB64), "")
		bytes := lenientBase64Decode(cleaned)
		blobOID := storeObject(repo, ObjBlob, bytes)
		ref := "refs/heads/" + branch
		tip, hasTip := repo.Refs[ref]
		tipCommit := ""
		if hasTip {
			tipCommit = peelToCommit(repo, tip)
		}
		var existing *FlatEntry
		if tipCommit != "" {
			if e, ok := flattenCommitTree(repo, tipCommit)[path]; ok {
				ee := e
				existing = &ee
			}
		}
		givenSha, hasGivenSha := strField(body["sha"])
		if existing != nil && (!hasGivenSha || givenSha != existing.OID) {
			return errorReply(409, "Conflict: sha does not match the current file sha")
		}
		commit := commitFileChange(store, repo, branch, path, &blobOID, message)
		status := 200
		if existing == nil {
			status = 201
		}
		name := path
		if idx := strings.LastIndex(path, "/"); idx != -1 {
			name = path[idx+1:]
		}
		return jsonReply(status, map[string]interface{}{
			"content": map[string]interface{}{
				"type": "file", "name": name, "path": path, "sha": blobOID,
				"size": len(bytes), "encoding": "base64", "content": base64Encode(bytes),
			},
			"commit": map[string]interface{}{"sha": commit, "message": message},
		}, nil)
	}

	if ctx.Method == "GET" {
		var ref string
		hasRef := false
		if vals, ok := ctx.Query["ref"]; ok && len(vals) > 0 {
			ref, hasRef = vals[0], true
		}
		tip := resolveCommitish(repo, ref, hasRef)
		if tip == "" {
			return errorReply(404, "Not Found")
		}
		treeOID := treeOfCommit(repo, tip)
		if treeOID == "" {
			return errorReply(404, "Not Found")
		}
		if path == "" {
			return jsonReply(200, dirListing(repo, treeOID, ""), nil)
		}
		parts := strings.Split(path, "/")
		currentTree := treeOID
		for i, part := range parts {
			treeObj, ok := repo.Objects[currentTree]
			if !ok || treeObj.Type != ObjTree {
				return errorReply(404, "Not Found")
			}
			var entry *TreeEntry
			for _, e := range parseTree(treeObj.Data) {
				if e.Name == part {
					ee := e
					entry = &ee
					break
				}
			}
			if entry == nil {
				return errorReply(404, "Not Found")
			}
			if i == len(parts)-1 {
				if entry.Mode == "40000" {
					return jsonReply(200, dirListing(repo, entry.OID, path), nil)
				}
				blob, ok := repo.Objects[entry.OID]
				if !ok {
					return errorReply(404, "Not Found")
				}
				return jsonReply(200, map[string]interface{}{
					"type": "file", "name": entry.Name, "path": path, "sha": entry.OID,
					"size": len(blob.Data), "encoding": "base64", "content": base64Encode(blob.Data),
				}, nil)
			}
			if entry.Mode != "40000" {
				return errorReply(404, "Not Found")
			}
			currentTree = entry.OID
		}
		return errorReply(404, "Not Found")
	}

	if ctx.Method == "DELETE" {
		message := "Delete file"
		if m, ok := strField(body["message"]); ok {
			message = m
		}
		branch := repo.DefaultBranch
		if b, ok := strField(body["branch"]); ok {
			branch = b
		}
		ref := "refs/heads/" + branch
		tip, hasTip := repo.Refs[ref]
		tipCommit := ""
		if hasTip {
			tipCommit = peelToCommit(repo, tip)
		}
		var existing *FlatEntry
		if tipCommit != "" {
			if e, ok := flattenCommitTree(repo, tipCommit)[path]; ok {
				ee := e
				existing = &ee
			}
		}
		if existing == nil {
			return errorReply(404, "Not Found")
		}
		if givenSha, has := strField(body["sha"]); has && givenSha != existing.OID {
			return errorReply(409, "Conflict: sha does not match the current file sha")
		}
		commit := commitFileChange(store, repo, branch, path, nil, message)
		return jsonReply(200, map[string]interface{}{
			"content": nil,
			"commit":  map[string]interface{}{"sha": commit, "message": message},
		}, nil)
	}

	return errorReply(404, "Not Found")
}

func dirListing(repo *Repo, treeOID string, basePath string) []interface{} {
	obj, ok := repo.Objects[treeOID]
	if !ok {
		return []interface{}{}
	}
	var out []interface{}
	for _, e := range parseTree(obj.Data) {
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		p := e.Name
		if basePath != "" {
			p = basePath + "/" + e.Name
		}
		out = append(out, map[string]interface{}{"type": typ, "name": e.Name, "path": p, "sha": e.OID})
	}
	if out == nil {
		out = []interface{}{}
	}
	return out
}

func handleIssues(ctx *Ctx, repo *Repo, seg []string, basePath string) Reply {
	store := ctx.Store

	if len(seg) == 0 {
		if ctx.Method == "POST" {
			body := ctx.bodyObj()
			title, ok := strField(body["title"])
			if !ok {
				return errorReply(422, "Validation Failed: title is required")
			}
			var labelNames []string
			if rawLabels, ok := body["labels"].([]interface{}); ok {
				for _, raw := range rawLabels {
					var name string
					if s, ok := raw.(string); ok {
						name = s
					} else if m, ok := raw.(map[string]interface{}); ok {
						name, _ = strField(m["name"])
					}
					if name == "" {
						continue
					}
					if _, exists := repo.Labels[name]; !exists {
						return errorReply(422, "Validation Failed: label \""+name+"\" does not exist")
					}
					labelNames = append(labelNames, name)
				}
			}
			bodyText, _ := strField(body["body"])
			issue := createIssue(store, repo, title, bodyText, labelNames)
			return issueReply(201, store, repo, issue, nil)
		}
		if ctx.Method == "GET" {
			state := "open"
			if vals, ok := ctx.Query["state"]; ok && len(vals) > 0 {
				state = vals[0]
			}
			var list []*Issue
			for _, i := range repo.Issues {
				list = append(list, i)
			}
			if state == "open" {
				list = filterIssues(list, "open")
			} else if state == "closed" {
				list = filterIssues(list, "closed")
			}
			sortIssuesAsc(list)
			direction := "desc"
			if vals, ok := ctx.Query["direction"]; ok && len(vals) > 0 {
				direction = vals[0]
			}
			if direction == "desc" {
				reverseIssues(list)
			}
			page, link := paginate(ctx, basePath+"/issues", list)
			var buf bytes.Buffer
			buf.WriteByte('[')
			for idx, i := range page {
				if idx > 0 {
					buf.WriteByte(',')
				}
				buf.Write(issueJSONBytes(store, repo, i))
			}
			buf.WriteByte(']')
			return Reply{Status: 200, Headers: linkHeaders(link), JSON: json.RawMessage(buf.Bytes()), HasJSON: true}
		}
		return errorReply(404, "Not Found")
	}

	number, isInt := segmentNumberToInt(seg[0])
	var issue *Issue
	if isInt {
		issue = repo.Issues[number]
	}

	if len(seg) == 1 {
		if issue == nil {
			return errorReply(404, "Not Found")
		}
		if ctx.Method == "GET" {
			return issueReply(200, store, repo, issue, nil)
		}
		if ctx.Method == "PATCH" {
			body := ctx.bodyObj()
			if body["state"] == "closed" || body["state"] == "open" {
				issue.State = body["state"].(string)
			}
			if s, ok := body["title"].(string); ok {
				issue.Title = s
			}
			if s, ok := body["body"].(string); ok {
				issue.Body = s
			}
			issue.UpdatedAt = store.nowIso()
			return issueReply(200, store, repo, issue, nil)
		}
		return errorReply(404, "Not Found")
	}

	if len(seg) == 2 && seg[1] == "comments" {
		_, hasPull := repo.Pulls[number]
		if issue == nil && !(isInt && hasPull) {
			return errorReply(404, "Not Found")
		}
		if ctx.Method == "POST" {
			body := ctx.bodyObj()
			text, isStr := body["body"].(string)
			if !isStr {
				return errorReply(422, "Validation Failed: body is required")
			}
			comment := &Comment{ID: store.NextComment, Body: text}
			store.NextComment++
			repo.Comments[number] = append(repo.Comments[number], comment)
			return jsonReply(201, map[string]interface{}{
				"id":         comment.ID,
				"body":       comment.Body,
				"user":       map[string]interface{}{"login": store.viewerLogin(), "id": 1000},
				"created_at": store.nowIso(),
				"updated_at": store.nowIso(),
			}, nil)
		}
		if ctx.Method == "GET" {
			list := repo.Comments[number]
			items := make([]interface{}, 0, len(list))
			for _, c := range list {
				items = append(items, map[string]interface{}{
					"id":         c.ID,
					"body":       c.Body,
					"user":       map[string]interface{}{"login": store.viewerLogin(), "id": 1000},
					"created_at": repo.CreatedAt,
					"updated_at": repo.UpdatedAt,
				})
			}
			return jsonReply(200, items, nil)
		}
		return errorReply(404, "Not Found")
	}

	return errorReply(404, "Not Found")
}

func filterIssues(list []*Issue, state string) []*Issue {
	var out []*Issue
	for _, i := range list {
		if i.State == state {
			out = append(out, i)
		}
	}
	return out
}

func sortIssuesAsc(list []*Issue) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Number < list[j-1].Number; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func reverseIssues(list []*Issue) {
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
}

func handlePulls(ctx *Ctx, repo *Repo, seg []string, basePath string) Reply {
	store := ctx.Store

	if len(seg) == 0 {
		if ctx.Method == "POST" {
			body := ctx.bodyObj()
			headRaw, okH := strField(body["head"])
			baseRaw, okB := strField(body["base"])
			title, okT := strField(body["title"])
			if !okH || !okB || !okT {
				return errorReply(422, "Validation Failed: head, base and title are required")
			}
			head := stripOwnerPrefix(headRaw)
			base := stripOwnerPrefix(baseRaw)
			if _, ok := repo.Refs["refs/heads/"+head]; !ok {
				return errorReply(422, "Validation Failed: head branch \""+head+"\" does not exist")
			}
			if _, ok := repo.Refs["refs/heads/"+base]; !ok {
				return errorReply(422, "Validation Failed: base branch \""+base+"\" does not exist")
			}
			for _, p := range repo.Pulls {
				if p.State == "open" && p.Head == head && p.Base == base {
					return errorReply(422, "Validation Failed: A pull request already exists for this head and base")
				}
			}
			bodyText, _ := strField(body["body"])
			pull := createPull(store, repo, title, bodyText, head, base)
			return jsonReply(201, pullJSON(store, repo, pull), nil)
		}
		if ctx.Method == "GET" {
			state := "open"
			if vals, ok := ctx.Query["state"]; ok && len(vals) > 0 {
				state = vals[0]
			}
			var list []*Pull
			for _, p := range repo.Pulls {
				list = append(list, p)
			}
			sortPullsAsc(list)
			if state == "open" {
				list = filterPulls(list, func(p *Pull) bool { return p.State == "open" })
			} else if state == "closed" {
				list = filterPulls(list, func(p *Pull) bool { return p.State == "closed" })
			}
			page, link := paginate(ctx, basePath+"/pulls", list)
			items := make([]interface{}, 0, len(page))
			for _, p := range page {
				items = append(items, pullJSON(store, repo, p))
			}
			return jsonReply(200, items, linkHeaders(link))
		}
		return errorReply(404, "Not Found")
	}

	number, isInt := segmentNumberToInt(seg[0])
	var pull *Pull
	if isInt {
		pull = repo.Pulls[number]
	}

	if len(seg) == 1 && ctx.Method == "GET" {
		if pull == nil {
			return errorReply(404, "Not Found")
		}
		return jsonReply(200, pullJSON(store, repo, pull), nil)
	}

	if len(seg) == 2 && seg[1] == "files" && ctx.Method == "GET" {
		if pull == nil {
			return errorReply(404, "Not Found")
		}
		changes := pullChanges(repo, pull)
		items := make([]interface{}, 0, len(changes))
		for _, c := range changes {
			items = append(items, map[string]interface{}{
				"filename":  c.Filename,
				"status":    c.Status,
				"additions": c.Additions,
				"deletions": c.Deletions,
				"changes":   c.Changes,
				"sha":       "",
			})
		}
		return jsonReply(200, items, nil)
	}

	if len(seg) == 2 && seg[1] == "merge" && ctx.Method == "PUT" {
		if pull == nil {
			return errorReply(404, "Not Found")
		}
		if pull.Merged {
			return errorReply(405, "Pull Request is not mergeable")
		}
		body := ctx.bodyObj()
		method := "squash"
		if m, _ := strField(body["merge_method"]); m == "merge" {
			method = "merge"
		}
		commit, outcome := mergePull(store, repo, pull, method)
		if outcome == "conflict" {
			return errorReply(405, "Pull Request is not mergeable: merge conflict")
		}
		if outcome == "null" {
			return errorReply(405, "Pull Request is not mergeable")
		}
		return jsonReply(200, map[string]interface{}{
			"sha":     commit,
			"merged":  true,
			"message": "Pull Request successfully merged",
		}, nil)
	}

	return errorReply(404, "Not Found")
}

func sortPullsAsc(list []*Pull) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Number < list[j-1].Number; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func filterPulls(list []*Pull, keep func(*Pull) bool) []*Pull {
	var out []*Pull
	for _, p := range list {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}
