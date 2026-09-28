-- approvals.session_id is an FK child with ON DELETE CASCADE but had no
-- index of its own, so a session delete's cascade (and any other lookup by
-- session_id) had to scan the whole approvals table, which is never pruned.
CREATE INDEX idx_approvals_session ON approvals(session_id);
