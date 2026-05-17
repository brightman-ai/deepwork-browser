package browser

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BrowserTaskStore manages BrowserTask persistence in memoryDB (knowledgeDB).
type BrowserTaskStore struct {
	db *sql.DB
}

// NewBrowserTaskStore creates a BrowserTaskStore and ensures the table exists.
func NewBrowserTaskStore(db *sql.DB) (*BrowserTaskStore, error) {
	s := &BrowserTaskStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// migrate creates browser_tasks table and indexes if not present.
func (s *BrowserTaskStore) migrate() error {
	ddl := `
CREATE TABLE IF NOT EXISTS browser_tasks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   INTEGER,
    workspace_id INTEGER,
    url          TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    screenshot   BLOB,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_browser_tasks_session
    ON browser_tasks(session_id);
CREATE INDEX IF NOT EXISTS idx_browser_tasks_status
    ON browser_tasks(status, created_at);
`
	_, err := s.db.Exec(ddl)
	return err
}

// Create inserts a new BrowserTask and returns the assigned ID.
func (s *BrowserTaskStore) Create(ctx context.Context, task *BrowserTask) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO browser_tasks (session_id, workspace_id, url, status, created_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, task.SessionID, task.WorkspaceID, task.URL, task.Status)
	if err != nil {
		return 0, fmt.Errorf("create browser_task: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// Get retrieves a BrowserTask by ID.
func (s *BrowserTaskStore) Get(ctx context.Context, id int64) (*BrowserTask, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, workspace_id, url, status, screenshot, created_at, completed_at
		FROM browser_tasks WHERE id = ?
	`, id)

	t := &BrowserTask{}
	var completedAt sql.NullTime
	err := row.Scan(
		&t.ID, &t.SessionID, &t.WorkspaceID, &t.URL,
		&t.Status, &t.Screenshot, &t.CreatedAt, &completedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("browser_task %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get browser_task: %w", err)
	}
	if completedAt.Valid {
		t.CompletedAt = &completedAt.Time
	}
	return t, nil
}

// UpdateStatus updates the task status and optionally sets completed_at.
func (s *BrowserTaskStore) UpdateStatus(ctx context.Context, id int64, status string, screenshot []byte) error {
	var err error
	if status == "completed" || status == "failed" {
		now := time.Now()
		_, err = s.db.ExecContext(ctx, `
			UPDATE browser_tasks
			SET status = ?, screenshot = ?, completed_at = ?
			WHERE id = ?
		`, status, screenshot, now, id)
	} else {
		_, err = s.db.ExecContext(ctx, `
			UPDATE browser_tasks SET status = ? WHERE id = ?
		`, status, id)
	}
	if err != nil {
		return fmt.Errorf("update browser_task status: %w", err)
	}
	return nil
}
