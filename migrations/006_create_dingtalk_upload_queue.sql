CREATE TABLE dingtalk_upload (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    checkin_id BIGINT NOT NULL COMMENT '本地签到记录ID',
    dingtalk_userid VARCHAR(128) NOT NULL COMMENT '钉钉员工UserID',
    user_check_time BIGINT NOT NULL COMMENT '钉钉打卡时间(毫秒)',
    status VARCHAR(16) NOT NULL DEFAULT 'pending' COMMENT 'pending/processing/success',
    attempts INT NOT NULL DEFAULT 0 COMMENT '上传尝试次数',
    next_attempt_at DATETIME NULL COMMENT '下次重试时间',
    request_id VARCHAR(128) NULL COMMENT '钉钉请求ID',
    last_error VARCHAR(500) NULL COMMENT '最近一次错误',
    uploaded_at DATETIME NULL COMMENT '上传成功时间',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_dingtalk_upload_checkin (checkin_id),
    INDEX idx_dingtalk_upload_pending (status, next_attempt_at),
    CONSTRAINT fk_dingtalk_upload_checkin FOREIGN KEY (checkin_id) REFERENCES checkin_log(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='钉钉签到上传队列';
