-- 为 checkin_log 增加设备标识列:同一设备 1 小时内只允许同一用户打卡
ALTER TABLE checkin_log
    ADD COLUMN device_id VARCHAR(128) NOT NULL DEFAULT '' COMMENT '设备唯一标识' AFTER point_id,
    ADD INDEX idx_checkin_log_device_created (device_id, created_at);
