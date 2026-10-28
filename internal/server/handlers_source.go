package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aloisdeniel/cairn/internal/store"
	"github.com/aloisdeniel/cairn/internal/versiondb"
)

// Read-only views the viewer's side panel needs: a version's published
// files, and the tables of its shared database.

// sourceFiles lists the files published in a version, sorted by path.
func (s *Server) sourceFiles(a *store.Artifact, v *store.Version) ([]fileInfo, error) {
	root := s.layout.ContentDir(a.ID, v.ContentDir)
	files := []fileInfo{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, fileInfo{Path: filepath.ToSlash(rel), Size: info.Size(), ModifiedAt: fileModTime(info.ModTime())})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

// handleSourceList lists a version's published files; the files themselves
// are served at /artifacts/{id}/{vid}/{path}.
func (s *Server) handleSourceList(w http.ResponseWriter, r *http.Request) {
	a := requestArtifact(r)
	v, err := s.store.VersionByID(a.ID, r.PathValue("vid"))
	if err != nil {
		s.writeStoreError(w, err, "version")
		return
	}
	files, err := s.sourceFiles(a, v)
	if err != nil {
		s.writeStoreError(w, err, "source")
		return
	}
	writeJSON(w, http.StatusOK, files)
}

type tableColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type tableInfo struct {
	Name    string        `json:"name"`
	Type    string        `json:"type"` // table or view
	Rows    int64         `json:"rows"`
	Columns []tableColumn `json:"columns"`
}

// handleDBTables describes a version's shared database: tables and views,
// their columns and row counts. It reads on the query_only pool, so it
// never creates a database that was never written.
func (s *Server) handleDBTables(w http.ResponseWriter, r *http.Request) {
	a := requestArtifact(r)
	v, err := s.store.VersionByID(a.ID, r.PathValue("vid"))
	if err != nil {
		s.writeStoreError(w, err, "version")
		return
	}
	query := func(sql string, params ...any) (*versiondb.Result, error) {
		return s.dbs.Exec(r.Context(), a.ID, v.ID, false, versiondb.Statement{SQL: sql, Params: params})
	}
	res, err := query(`SELECT name, type FROM sqlite_master WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tables := []tableInfo{}
	for _, row := range res.Rows {
		t := tableInfo{Name: fmt.Sprint(row[0]), Type: fmt.Sprint(row[1]), Columns: []tableColumn{}}
		if cols, err := query(`SELECT name, type FROM pragma_table_info(?)`, t.Name); err == nil {
			for _, c := range cols.Rows {
				t.Columns = append(t.Columns, tableColumn{Name: fmt.Sprint(c[0]), Type: fmt.Sprint(c[1])})
			}
		}
		if n, err := query(`SELECT COUNT(*) FROM "` + strings.ReplaceAll(t.Name, `"`, `""`) + `"`); err == nil && len(n.Rows) == 1 {
			fmt.Sscan(fmt.Sprint(n.Rows[0][0]), &t.Rows)
		}
		tables = append(tables, t)
	}
	writeJSON(w, http.StatusOK, tables)
}
