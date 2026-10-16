-- 把未设置密码的用户密码初始化为 123456;已有密码的用户不受影响。
-- 钉钉通讯录同步导入的用户密码是哨兵值(见 dingtalk.go 的 importedDingTalkPasswordHash),一并初始化。
-- 哈希由 `go run . hash-password` 生成,不要把明文密码写入数据库。
UPDATE user
SET password_hash = 'pbkdf2_sha256$600000$Pooj0IMwWZw+Fnz77lhpow$/Q3erN/utxNImuroEA2R/aQ9CjLAfHtK4vJ/6Tm+suw'
WHERE password_hash = '' OR password_hash = '!dingtalk-imported-account-password-not-set';
