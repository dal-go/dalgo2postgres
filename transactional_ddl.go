package dalgo2postgres

// SupportsTransactionalDDL reports that PostgreSQL supports transactional
// DDL — every CREATE / DROP / ALTER statement can be wrapped in a
// BEGIN/COMMIT and is rolled back atomically on commit failure.
func (d *Database) SupportsTransactionalDDL() bool { return true }
