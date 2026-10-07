ALTER TABLE user
    ADD COLUMN password_hash VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'PBKDF2密码哈希' AFTER department,
    ADD COLUMN status TINYINT(1) NOT NULL DEFAULT 1 COMMENT '1启用, 0禁用' AFTER password_hash;

-- 为已有用户设置 PBKDF2 哈希后才能登录；不要把明文密码写入数据库。
