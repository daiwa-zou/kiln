-- Browser-uploaded source documents. Files are workspace-scoped rather than
-- connector-scoped so the material survives connector recreation: the upload
-- connector in files mode consumes every row here, and deleting the connector
-- is a configuration change, not a data loss.
--
-- path is the sanitized relative path a file materializes under at sync time
-- (and the unit key the pipeline derives); blob_key names the bytes in object
-- storage and is server-constructed, never user input. Re-uploading a path
-- replaces the row; the superseded blob is deleted by the caller after commit.

CREATE TABLE workspace_files (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    path         TEXT NOT NULL,
    blob_key     TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL,
    content_type TEXT,
    sha256       TEXT NOT NULL,
    uploaded_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, path)
);
CREATE INDEX workspace_files_workspace_idx ON workspace_files (workspace_id);
