-- Keep both DingTalk organizations in the same local database.
-- Existing users and queued uploads remain assigned to the original org.
ALTER TABLE user
    ADD COLUMN dingtalk_org VARCHAR(32) NOT NULL DEFAULT 'org_lezhiedu'
        COMMENT '钉钉组织标识' AFTER dingtalk_userid;

ALTER TABLE user
    DROP INDEX uk_mobile,
    DROP INDEX uk_dingtalk_userid,
    ADD UNIQUE KEY uk_org_mobile (dingtalk_org, mobile),
    ADD UNIQUE KEY uk_org_dingtalk_userid (dingtalk_org, dingtalk_userid);

UPDATE user SET dingtalk_org='org_lezhiedu';

ALTER TABLE dingtalk_upload
    ADD COLUMN dingtalk_org VARCHAR(32) NOT NULL DEFAULT 'org_lezhiedu'
        COMMENT '钉钉组织标识' AFTER upload_sequence,
    ADD INDEX idx_dingtalk_upload_org_pending (dingtalk_org, status, next_attempt_at);
