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

ALTER TABLE checkin_log
    ADD COLUMN distance_m DECIMAL(10,2) NOT NULL DEFAULT 0 COMMENT '打卡时距签到点距离(米)' AFTER latitude,
    ADD CONSTRAINT fk_checkin_log_user FOREIGN KEY (user_id) REFERENCES user(id),
    ADD CONSTRAINT fk_checkin_log_point FOREIGN KEY (point_id) REFERENCES checkin_point(id);
