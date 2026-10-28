package store

import (
	"database/sql"

	"github.com/google/uuid"
)

// Comment is a message on an artifact. Root comments (ParentID empty) start
// threads; replies point at their root. Resolution lives on the root.
type Comment struct {
	ID         string `json:"id"`
	ArtifactID string `json:"artifactId"`
	VersionID  string `json:"versionId,omitempty"` // version the author was looking at
	ParentID   string `json:"parentId,omitempty"`
	UserID     string `json:"userId,omitempty"` // empty once the author is deleted
	Author     string `json:"author"`           // the author's name, else email
	Via        string `json:"via"`              // web, api or mcp
	Body       string `json:"body"`
	ResolvedAt string `json:"resolvedAt,omitempty"`
	ResolvedBy string `json:"resolvedBy,omitempty"` // user id
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

const commentSelect = `SELECT c.id, c.artifact_id, COALESCE(c.version_id, ''), COALESCE(c.parent_id, ''), COALESCE(c.user_id, ''),
	COALESCE(NULLIF(u.name, ''), u.email, ''), c.via, c.body, COALESCE(c.resolved_at, ''), COALESCE(c.resolved_by, ''),
	c.created_at, c.updated_at
	FROM comments c LEFT JOIN users u ON u.id = c.user_id`

func scanComment(row interface{ Scan(...any) error }) (*Comment, error) {
	var c Comment
	if err := row.Scan(&c.ID, &c.ArtifactID, &c.VersionID, &c.ParentID, &c.UserID, &c.Author, &c.Via, &c.Body,
		&c.ResolvedAt, &c.ResolvedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) CreateComment(c *Comment) (*Comment, error) {
	t := now()
	c.ID, c.CreatedAt, c.UpdatedAt = uuid.NewString(), t, t
	_, err := s.db.Exec(`INSERT INTO comments (id, artifact_id, version_id, parent_id, user_id, via, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ArtifactID, nullable(c.VersionID), nullable(c.ParentID), nullable(c.UserID), c.Via, c.Body, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return s.CommentByID(c.ArtifactID, c.ID)
}

func (s *Store) CommentByID(artifactID, id string) (*Comment, error) {
	return scanComment(s.db.QueryRow(commentSelect+` WHERE c.id = ? AND c.artifact_id = ?`, id, artifactID))
}

// ListComments returns an artifact's comments, oldest first. With
// status "open" or "resolved", only the threads (roots and their replies)
// in that state.
func (s *Store) ListComments(artifactID, status string) ([]*Comment, error) {
	q := commentSelect + ` WHERE c.artifact_id = ?`
	switch status {
	case "open":
		q += ` AND (SELECT resolved_at FROM comments r WHERE r.id = COALESCE(c.parent_id, c.id)) IS NULL`
	case "resolved":
		q += ` AND (SELECT resolved_at FROM comments r WHERE r.id = COALESCE(c.parent_id, c.id)) IS NOT NULL`
	}
	rows, err := s.db.Query(q+` ORDER BY c.created_at, c.rowid`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Comment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpdateCommentBody(artifactID, id, body string) error {
	return s.exec1(`UPDATE comments SET body = ?, updated_at = ? WHERE id = ? AND artifact_id = ?`, body, now(), id, artifactID)
}

// ResolveComment resolves (or reopens, with an empty userID) a thread root.
func (s *Store) ResolveComment(artifactID, id, userID string) error {
	var at sql.NullString
	if userID != "" {
		at = sql.NullString{String: now(), Valid: true}
	}
	return s.exec1(`UPDATE comments SET resolved_at = ?, resolved_by = ? WHERE id = ? AND artifact_id = ? AND parent_id IS NULL`,
		at, nullable(userID), id, artifactID)
}

func (s *Store) DeleteComment(artifactID, id string) error {
	return s.exec1(`DELETE FROM comments WHERE id = ? AND artifact_id = ?`, id, artifactID)
}
