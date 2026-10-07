ALTER TABLE checkin_log
    ADD INDEX idx_checkin_log_user_created (user_id, created_at, id);
