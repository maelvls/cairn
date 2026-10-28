-- Anchored comments: a thread root may point at an element of the page it is
-- about. JSON {selector, quote, path} (see store.CommentAnchor); NULL for
-- comments on the whole artifact and for every reply.
ALTER TABLE comments ADD COLUMN anchor TEXT;
