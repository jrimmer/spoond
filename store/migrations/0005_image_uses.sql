-- Lifetime lease grants per image, for the dashboard's catalog. Leases
-- are deleted when they end, so this is the only durable count. It is
-- kept apart from images so an image upsert never resets it, and it is
-- seeded from the leases alive when the migration runs.
CREATE TABLE image_uses (
  image TEXT PRIMARY KEY,
  uses  INTEGER NOT NULL DEFAULT 0
);
INSERT INTO image_uses (image, uses) SELECT image, COUNT(*) FROM leases GROUP BY image;
