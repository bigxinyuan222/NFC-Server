DROP DATABASE IF EXISTS checkin_db;
CREATE DATABASE checkin_db DEFAULT CHARSET utf8mb4;

USE checkin_db;

CREATE TABLE user (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(64) NOT NULL COMMENT '姓名',
    department VARCHAR(64) NOT NULL DEFAULT '' COMMENT '部门',
    password_hash VARCHAR(255) NOT NULL COMMENT 'PBKDF2密码哈希',
    mobile VARCHAR(32) NULL COMMENT '员工手机号',
    dingtalk_userid VARCHAR(128) NULL COMMENT '钉钉员工UserID',
    status TINYINT(1) NOT NULL DEFAULT 1 COMMENT '1启用, 0禁用',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_name (name),
    UNIQUE KEY uk_mobile (mobile),
    UNIQUE KEY uk_dingtalk_userid (dingtalk_userid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户';

CREATE TABLE checkin_point (
    id VARCHAR(64) PRIMARY KEY COMMENT '签到点ID',
    name VARCHAR(128) NOT NULL COMMENT '签到点名称',
    longitude DECIMAL(10,7) NOT NULL COMMENT '签到点经度',
    latitude DECIMAL(10,7) NOT NULL COMMENT '签到点纬度',
    status TINYINT(1) NOT NULL DEFAULT 1 COMMENT '1启用, 0禁用',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT chk_checkin_point_longitude CHECK (longitude >= -180 AND longitude <= 180),
    CONSTRAINT chk_checkin_point_latitude CHECK (latitude >= -90 AND latitude <= 90)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='签到点';

INSERT INTO checkin_point (id, name, longitude, latitude, status)
VALUES ('point-001', '现场签到点', 113.9475000, 35.2952778, 1);

CREATE TABLE checkin_log (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL COMMENT '用户ID',
    point_id VARCHAR(64) NOT NULL COMMENT '打卡点ID',
    device_id VARCHAR(128) NOT NULL DEFAULT '' COMMENT '设备唯一标识',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '打卡时间',
    INDEX idx_user_point (user_id, point_id),
    INDEX idx_checkin_log_user_created (user_id, created_at, id),
    INDEX idx_checkin_log_device_created (device_id, created_at),
    CONSTRAINT fk_checkin_log_user FOREIGN KEY (user_id) REFERENCES user(id),
    CONSTRAINT fk_checkin_log_point FOREIGN KEY (point_id) REFERENCES checkin_point(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='打卡记录';

CREATE TABLE dingtalk_upload (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    checkin_id BIGINT NOT NULL COMMENT '本地签到记录ID',
    upload_sequence TINYINT UNSIGNED NOT NULL COMMENT '同一次本地签到的钉钉流水序号',
    dingtalk_userid VARCHAR(128) NOT NULL COMMENT '钉钉员工UserID',
    user_check_time BIGINT NOT NULL COMMENT '钉钉打卡时间(毫秒)',
    status VARCHAR(16) NOT NULL DEFAULT 'pending' COMMENT 'pending/processing/success',
    attempts INT NOT NULL DEFAULT 0 COMMENT '上传尝试次数',
    next_attempt_at DATETIME NULL COMMENT '下次重试时间',
    request_id VARCHAR(128) NULL COMMENT '钉钉请求ID',
    last_error VARCHAR(500) NULL COMMENT '最近一次错误',
    uploaded_at DATETIME NULL COMMENT '上传成功时间',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_dingtalk_upload_checkin (checkin_id, upload_sequence),
    INDEX idx_dingtalk_upload_pending (status, next_attempt_at),
    CONSTRAINT fk_dingtalk_upload_checkin FOREIGN KEY (checkin_id) REFERENCES checkin_log(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='钉钉签到上传队列';
