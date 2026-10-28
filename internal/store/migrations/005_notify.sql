-- Comment notifications over ntfy (see internal/server/notify.go): each
-- user picks their own topic; NULL means no notifications.
ALTER TABLE users ADD COLUMN ntfy_topic TEXT;
