ALTER TABLE user
    ADD COLUMN mobile VARCHAR(32) NULL COMMENT '员工手机号' AFTER password_hash,
    ADD UNIQUE KEY uk_mobile (mobile);
