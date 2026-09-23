-- Keep the key on rollback: an older binary that reads channel_installation
-- config still has to open the ciphertext this key sealed.
SELECT 1;
