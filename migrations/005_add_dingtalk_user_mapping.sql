ALTER TABLE user
    ADD COLUMN dingtalk_userid VARCHAR(128) NULL COMMENT '钉钉员工UserID' AFTER password_hash,
    ADD UNIQUE KEY uk_dingtalk_userid (dingtalk_userid);
