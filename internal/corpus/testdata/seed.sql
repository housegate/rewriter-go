SELECT a FROM db.t WHERE x IN (1, 2, 3)
---
SELECT * FROM a GLOBAL JOIN b ON a.id = b.id
---
INSERT INTO db.t (a) VALUES (1)
---
CREATE TABLE db.t (a Int64) ENGINE = MergeTree ORDER BY a
---
DROP TABLE IF EXISTS db.t
---
ALTER TABLE db.t ADD COLUMN b Int64
---
RENAME TABLE db.a TO db.b
---
USE db
---
SHOW TABLES FROM db
---
SHOW CREATE TABLE db.t
---
EXISTS TABLE db.t
---
GRANT SELECT ON db.t TO u
