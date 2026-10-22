-- An image's capability and one-line description (#52), the manifest's
-- `capability` and `description` fields: written by `spoond images
-- build`, or into existing rows by `spoond images describe`, and served
-- by GET /api/capabilities. The image's builder owns them; spoond keeps
-- no table of its own. '' = unset: the document omits the field.
ALTER TABLE images ADD COLUMN capability TEXT NOT NULL DEFAULT '';
ALTER TABLE images ADD COLUMN description TEXT NOT NULL DEFAULT '';
