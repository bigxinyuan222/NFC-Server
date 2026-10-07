INSERT INTO checkin_point (id, name, longitude, latitude, status)
VALUES ('point-001', '现场签到点', 113.9475000, 35.2952778, 1)
ON DUPLICATE KEY UPDATE
    name = VALUES(name),
    longitude = VALUES(longitude),
    latitude = VALUES(latitude),
    status = VALUES(status);
