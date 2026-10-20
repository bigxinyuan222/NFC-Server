ALTER TABLE dingtalk_upload
    ADD COLUMN upload_sequence TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '同一次本地签到的钉钉流水序号' AFTER checkin_id,
    DROP INDEX uk_dingtalk_upload_checkin,
    ADD UNIQUE KEY uk_dingtalk_upload_checkin (checkin_id, upload_sequence);
