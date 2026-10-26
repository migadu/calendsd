-- Every calendar object carries its etag (docs/slow-queries.md, item 3).
--
-- Sabre's PDO backend and this server both store the md5 of the payload as
-- the etag on every write, but the column is nullable and rows imported from
-- elsewhere can lack one; the read path covered that with
-- coalesce(etag, md5(calendardata)), which makes every listing read the
-- payload. With the gap filled once here, the metadata-only listing can
-- select the etag column alone. On a table with many NULL etags, run the
-- UPDATE in id ranges beforehand; this statement then finds nothing to do.
UPDATE calendarobjects SET etag = md5(calendardata) WHERE etag IS NULL AND calendardata IS NOT NULL;
