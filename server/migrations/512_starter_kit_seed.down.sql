-- Dropping the marker would let the next start re-import the bundle into every
-- workspace, recreating skills and agents their admins have since renamed or
-- deleted. Roll back the kit import in application code first if that is really
-- wanted; the schema of the imported rows never changed.
SELECT 1;
