package store

import (
	"github.com/google/uuid"
)

// Artifact is a named collection of versions.
type Artifact struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Public      bool        `json:"public"`
	CreatedAt   string      `json:"createdAt"`
	UpdatedAt   string      `json:"updatedAt"`
	CreatedBy   string      `json:"createdBy,omitempty"` // user id
	Resources   []*Resource `json:"resources,omitempty"`
}

// Resource associates external references (e.g. a Claude session ID) with an
// artifact.
type Resource struct {
	ID         string `json:"id"`
	ArtifactID string `json:"-"`
	Type       string `json:"type"`
	Value      string `json:"value"`
}

// Version is one uploaded revision of an artifact. ContentDir is the name of
// the extracted directory under content/{artifactID}/; it changes on every
// (re-)upload so replacement is an atomic pointer swap.
type Version struct {
	ID         string `json:"id"`
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
	Changelog  string `json:"changelog"`
	Seq        int    `json:"seq"`
	ContentDir string `json:"-"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
	CreatedBy  string `json:"createdBy,omitempty"` // user id
}

const artifactCols = `id, name, description, public, created_at, updated_at, created_by`

func scanArtifact(row interface{ Scan(...any) error }) (*Artifact, error) {
	var a Artifact
	if err := row.Scan(&a.ID, &a.Name, &a.Description, &a.Public, &a.CreatedAt, &a.UpdatedAt, &a.CreatedBy); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateArtifact creates an artifact; createdBy is the creator's user id.
func (s *Store) CreateArtifact(name, description string, public bool, createdBy string) (*Artifact, error) {
	t := now()
	a := &Artifact{ID: uuid.NewString(), Name: name, Description: description, Public: public, CreatedAt: t, UpdatedAt: t, CreatedBy: createdBy}
	_, err := s.db.Exec(`INSERT INTO artifacts (id, name, description, public, created_at, updated_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.Description, a.Public, a.CreatedAt, a.UpdatedAt, a.CreatedBy)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Store) ArtifactByID(id string) (*Artifact, error) {
	return scanArtifact(s.db.QueryRow(`SELECT `+artifactCols+` FROM artifacts WHERE id = ?`, id))
}

// ArtifactByName returns the artifact with the given exact name. Names are not
// unique; the oldest match wins (used by the CLI for convenience).
func (s *Store) ArtifactByName(name string) (*Artifact, error) {
	return scanArtifact(s.db.QueryRow(`SELECT `+artifactCols+` FROM artifacts WHERE name = ? ORDER BY created_at LIMIT 1`, name))
}

// ArtifactsByResource returns the artifacts associated with a resource,
// matched by resource value (e.g. a Claude session id) or resource row id.
func (s *Store) ArtifactsByResource(ref string) ([]*Artifact, error) {
	rows, err := s.db.Query(`SELECT DISTINCT a.id, a.name, a.description, a.public, a.created_at, a.updated_at, a.created_by
		FROM artifacts a
		JOIN artifact_resources r ON r.artifact_id = a.id
		WHERE r.value = ? OR r.id = ?
		ORDER BY a.created_at`, ref, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListArtifacts returns every artifact, most recently updated first.
func (s *Store) ListArtifacts() ([]*Artifact, error) {
	rows, err := s.db.Query(`SELECT ` + artifactCols + ` FROM artifacts ORDER BY updated_at DESC, created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ArtifactSummary is an artifact as the gallery shows it: its latest
// version, counts, and whether the viewing user pinned it.
type ArtifactSummary struct {
	*Artifact
	LatestVersion *Version `json:"latestVersion,omitempty"`
	VersionCount  int      `json:"versionCount"`
	OpenComments  int      `json:"openComments"`
	Pinned        bool     `json:"pinned"`
}

// ArtifactSummaries returns every artifact with its latest version, version
// and open-thread counts, and userID's pins, most recently updated first.
func (s *Store) ArtifactSummaries(userID string) ([]*ArtifactSummary, error) {
	rows, err := s.db.Query(`SELECT a.id, a.name, a.description, a.public, a.created_at, a.updated_at, a.created_by,
			(SELECT COUNT(*) FROM versions v WHERE v.artifact_id = a.id),
			(SELECT COUNT(*) FROM comments c WHERE c.artifact_id = a.id AND c.parent_id IS NULL AND c.resolved_at IS NULL),
			EXISTS (SELECT 1 FROM pins p WHERE p.artifact_id = a.id AND p.user_id = ?),
			l.id, l.name, l.changelog, l.seq, l.created_at, l.updated_at, l.created_by
		FROM artifacts a
		LEFT JOIN versions l ON l.artifact_id = a.id AND l.seq = (SELECT MAX(seq) FROM versions WHERE artifact_id = a.id)
		ORDER BY a.updated_at DESC, a.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ArtifactSummary
	for rows.Next() {
		a := &Artifact{}
		sum := &ArtifactSummary{Artifact: a}
		var lid, lname, lchangelog, lcreated, lupdated, lby *string
		var lseq *int
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.Public, &a.CreatedAt, &a.UpdatedAt, &a.CreatedBy,
			&sum.VersionCount, &sum.OpenComments, &sum.Pinned,
			&lid, &lname, &lchangelog, &lseq, &lcreated, &lupdated, &lby); err != nil {
			return nil, err
		}
		if lid != nil {
			sum.LatestVersion = &Version{ID: *lid, ArtifactID: a.ID, Name: *lname, Changelog: *lchangelog,
				Seq: *lseq, CreatedAt: *lcreated, UpdatedAt: *lupdated, CreatedBy: *lby}
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

func (s *Store) UpdateArtifact(id, name, description string, public bool) error {
	return s.exec1(`UPDATE artifacts SET name = ?, description = ?, public = ?, updated_at = ? WHERE id = ?`,
		name, description, public, now(), id)
}

func (s *Store) DeleteArtifact(id string) error {
	return s.exec1(`DELETE FROM artifacts WHERE id = ?`, id)
}

func (s *Store) touchArtifact(id string) {
	s.db.Exec(`UPDATE artifacts SET updated_at = ? WHERE id = ?`, now(), id)
}

// Pins are per user: each user pins the artifacts they keep coming back to.

func (s *Store) Pin(userID, artifactID string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO pins (user_id, artifact_id, created_at) VALUES (?, ?, ?)`, userID, artifactID, now())
	return err
}

func (s *Store) Unpin(userID, artifactID string) error {
	_, err := s.db.Exec(`DELETE FROM pins WHERE user_id = ? AND artifact_id = ?`, userID, artifactID)
	return err
}

func (s *Store) IsPinned(userID, artifactID string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM pins WHERE user_id = ? AND artifact_id = ?`, userID, artifactID).Scan(&n)
	return n > 0
}

// Resources

func (s *Store) AddResource(artifactID, typ, value string) (*Resource, error) {
	r := &Resource{ID: uuid.NewString(), ArtifactID: artifactID, Type: typ, Value: value}
	_, err := s.db.Exec(`INSERT INTO artifact_resources (id, artifact_id, type, value) VALUES (?, ?, ?, ?)`,
		r.ID, r.ArtifactID, r.Type, r.Value)
	if err != nil {
		return nil, err
	}
	s.touchArtifact(artifactID)
	return r, nil
}

func (s *Store) ListResources(artifactID string) ([]*Resource, error) {
	rows, err := s.db.Query(`SELECT id, artifact_id, type, value FROM artifact_resources WHERE artifact_id = ?`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Resource
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.ID, &r.ArtifactID, &r.Type, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteResource(artifactID, resourceID string) error {
	return s.exec1(`DELETE FROM artifact_resources WHERE id = ? AND artifact_id = ?`, resourceID, artifactID)
}

// Versions

const versionCols = `id, artifact_id, name, changelog, seq, content_dir, created_at, updated_at, created_by`

func scanVersion(row interface{ Scan(...any) error }) (*Version, error) {
	var v Version
	if err := row.Scan(&v.ID, &v.ArtifactID, &v.Name, &v.Changelog, &v.Seq, &v.ContentDir, &v.CreatedAt, &v.UpdatedAt, &v.CreatedBy); err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateVersion adds a version; createdBy is the publisher's user id.
func (s *Store) CreateVersion(artifactID, name, changelog, contentDir, createdBy string) (*Version, error) {
	t := now()
	v := &Version{ID: uuid.NewString(), ArtifactID: artifactID, Name: name, Changelog: changelog, ContentDir: contentDir, CreatedAt: t, UpdatedAt: t, CreatedBy: createdBy}
	err := s.db.QueryRow(`INSERT INTO versions (id, artifact_id, name, changelog, seq, content_dir, created_at, updated_at, created_by)
		VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM versions WHERE artifact_id = ?), ?, ?, ?, ?)
		RETURNING seq`,
		v.ID, v.ArtifactID, v.Name, v.Changelog, artifactID, v.ContentDir, v.CreatedAt, v.UpdatedAt, v.CreatedBy).Scan(&v.Seq)
	if err != nil {
		return nil, err
	}
	s.touchArtifact(artifactID)
	return v, nil
}

func (s *Store) VersionByID(artifactID, versionID string) (*Version, error) {
	return scanVersion(s.db.QueryRow(`SELECT `+versionCols+` FROM versions WHERE id = ? AND artifact_id = ?`, versionID, artifactID))
}

func (s *Store) LatestVersion(artifactID string) (*Version, error) {
	return scanVersion(s.db.QueryRow(`SELECT `+versionCols+` FROM versions WHERE artifact_id = ? ORDER BY seq DESC LIMIT 1`, artifactID))
}

func (s *Store) ListVersions(artifactID string) ([]*Version, error) {
	rows, err := s.db.Query(`SELECT `+versionCols+` FROM versions WHERE artifact_id = ? ORDER BY seq DESC`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SwapVersionContent atomically points a version at a freshly extracted
// content dir (re-upload). Returns the previous content dir for cleanup.
func (s *Store) SwapVersionContent(artifactID, versionID, contentDir, name, changelog string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var prev string
	if err := tx.QueryRow(`SELECT content_dir FROM versions WHERE id = ? AND artifact_id = ?`, versionID, artifactID).Scan(&prev); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE versions SET content_dir = ?, name = ?, changelog = ?, updated_at = ? WHERE id = ?`,
		contentDir, name, changelog, now(), versionID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.touchArtifact(artifactID)
	return prev, nil
}

// UpdateVersionMeta renames a version or edits its changelog. It leaves
// updated_at alone: that one tells when the version's files last changed
// (viewers reload the page when it moves).
func (s *Store) UpdateVersionMeta(artifactID, versionID, name, changelog string) error {
	return s.exec1(`UPDATE versions SET name = ?, changelog = ? WHERE id = ? AND artifact_id = ?`,
		name, changelog, versionID, artifactID)
}

func (s *Store) DeleteVersion(artifactID, versionID string) error {
	return s.exec1(`DELETE FROM versions WHERE id = ? AND artifact_id = ?`, versionID, artifactID)
}
