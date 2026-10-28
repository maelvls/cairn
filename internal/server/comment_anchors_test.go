package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aloisdeniel/cairn/internal/store"
)

func TestCommentAnchors(t *testing.T) {
	_, ts := testServer(t)
	admin, aid, vid := setupArtifact(t, ts.URL, false)
	base := "/api/artifacts/" + aid + "/comments"

	// Whitespace in the quote collapses; the page path loses its slash.
	var root store.Comment
	admin.mustDo("POST", base, map[string]any{"body": "bigger", "versionId": vid, "anchor": map[string]any{
		"selector": " main > h1 ", "quote": "  Results\n  so far ", "path": "/index.html",
	}}, &root, http.StatusCreated)
	want := store.CommentAnchor{Selector: "main > h1", Quote: "Results so far", Path: "index.html"}
	if root.Anchor == nil || *root.Anchor != want {
		t.Fatalf("anchor: %+v", root.Anchor)
	}

	// Replies never carry anchors; anchors carry something to find.
	admin.mustDo("POST", base, map[string]any{"body": "x", "parentId": root.ID, "anchor": map[string]any{"selector": "h1"}}, nil, http.StatusBadRequest)
	admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{}}, nil, http.StatusBadRequest)
	admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{"path": "index.html", "quote": "  "}}, nil, http.StatusBadRequest)
	admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{"quote": strings.Repeat("é", 301)}}, nil, http.StatusBadRequest)
	admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{"selector": strings.Repeat("a", 2001)}}, nil, http.StatusBadRequest)
	admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{"quote": "q", "path": strings.Repeat("a", 1025)}}, nil, http.StatusBadRequest)
	// The page stays inside the version: the viewer loads it in its frame.
	for _, p := range []string{"../../other/v/", "a/../../b", "a//b", `..\..\api`, "./x"} {
		admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{"quote": "q", "path": p}}, nil, http.StatusBadRequest)
	}
	var sub store.Comment
	admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{"quote": "q", "path": "about/"}}, &sub, http.StatusCreated)
	if sub.Anchor.Path != "about/" {
		t.Errorf("sub-page path: %+v", sub.Anchor)
	}
	admin.mustDo("DELETE", base+"/"+sub.ID, nil, nil, http.StatusOK)
	// Without a path, the anchor is on the main page.
	var quoteOnly store.Comment
	admin.mustDo("POST", base, map[string]any{"body": "x", "anchor": map[string]any{"quote": strings.Repeat("é", 300)}}, &quoteOnly, http.StatusCreated)
	if quoteOnly.Anchor.Path != "index.html" {
		t.Errorf("default path: %+v", quoteOnly.Anchor)
	}

	var reply store.Comment
	admin.mustDo("POST", base, map[string]any{"body": "done", "parentId": root.ID}, &reply, http.StatusCreated)
	var all []store.Comment
	admin.mustDo("GET", base, nil, &all, http.StatusOK)
	if len(all) != 3 || all[0].Anchor == nil || *all[0].Anchor != want || all[1].Anchor == nil || all[2].Anchor != nil {
		t.Fatalf("listed: %+v", all)
	}
	// Comments without an anchor have no anchor field at all.
	if n := strings.Count(body(t, get(t, ts.URL+base, admin.token, "")), `"anchor":`); n != 2 {
		t.Errorf(`%d "anchor" fields, want 2`, n)
	}
	// Edits keep the anchor.
	admin.mustDo("PATCH", base+"/"+root.ID, map[string]any{"body": "much bigger", "anchor": nil}, &root, http.StatusOK)
	if root.Anchor == nil || *root.Anchor != want {
		t.Errorf("anchor after an edit: %+v", root.Anchor)
	}
}

func TestMCPCommentAnchors(t *testing.T) {
	_, ts := testServer(t)
	admin, aid, _ := setupArtifact(t, ts.URL, false)

	var root store.Comment
	callTool(t, admin, "add_comment", map[string]any{"artifact": aid, "body": "the title is cut",
		"anchor": map[string]any{"selector": "h1", "quote": "Hello"}}, &root, false)
	if root.Anchor == nil || root.Anchor.Selector != "h1" || root.Anchor.Quote != "Hello" || root.Anchor.Path != "index.html" || root.Via != "mcp" {
		t.Fatalf("anchored root: %+v", root)
	}
	text := callTool(t, admin, "add_comment", map[string]any{"artifact": aid, "body": "fixed", "reply_to": root.ID,
		"anchor": map[string]any{"selector": "h1"}}, nil, true)
	if !strings.Contains(text, "anchor") {
		t.Errorf("reply with an anchor: %s", text)
	}
	var threads []commentThread
	callTool(t, admin, "list_comments", map[string]any{"artifact": aid}, &threads, false)
	if len(threads) != 1 || threads[0].Anchor == nil || threads[0].Anchor.Quote != "Hello" {
		t.Fatalf("threads: %+v", threads)
	}
}
